// Package server holds encrypted blobs for the owners of a storage
// citizen. It cannot read them.
//
// An owner is an account key: a key that the signer of a citizen derives
// for this one storage citizen. The account key asks for an account with
// wire.JoinCall, and the rule of admission decides. It then sends
// wire.Grants, the device keys that can use the account, and ends an empty
// account with wire.CloseCall. A request is one ARC call,
// wire.Open, with a WebRTC offer, from the account key or from a device
// key with a grant. The reply is wire.Answer, with the
// WebRTC answer. The two programs then open one pipe of sdk/direct, and
// the client sends hello, put, list, get, delete, usage, audit and drain
// requests on it, several in flight. No blob goes through ARC.
package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/pion/webrtc/v4"
)

const (
	// MaxSessions is the number of pipes that the app holds at a time.
	MaxSessions = 4
	// IdleWait closes a pipe that carries no message for this long.
	IdleWait = 30 * time.Second
	// DefaultQuota is the quota of an account when STORAGE_QUOTA is not
	// set.
	DefaultQuota = 1 << 30
)

// The errors of the ARC call.
const (
	errInvalidRequest = provider.Error("invalid_request")
	errNotOwner       = provider.Error("not_owner")
	errBusy           = provider.Error("busy")
	errFailed         = provider.Error("storage_failed")
)

type server struct {
	store *store
	rules rules
	// self is the key of this storage citizen, in hex. A join that shows
	// the citizen key signs it.
	self  string
	rtc   direct.Options
	slots chan struct{}
	log   io.Writer

	// mu covers pipes and holds.
	mu sync.Mutex
	// pipes holds the end of each open pipe, by account, so that the
	// removal of an account ends its pipes.
	pipes map[string]map[*func()]struct{}
	// holds has one lock for each account. Each session holds it to read,
	// and the removal of the account holds it to write.
	holds map[string]*sync.RWMutex
}

// Run serves the storage app through core ARC. The environment gives the
// settings:
//
//	STORAGE_DIR       the directory of the blobs
//	STORAGE_OWNERS    the account keys, in hex, that can open an account,
//	                  separated by spaces or commas. arc keys account
//	                  prints the account key of a citizen for this
//	                  storage citizen.
//	STORAGE_QUOTA     the quota of each account of STORAGE_OWNERS, such
//	                  as 512MiB or 1GiB
//	STORAGE_ADMIT     the rule of admission of a join: invite (the
//	                  default), ask or open
//	STORAGE_OPEN_QUOTA  the quota of an account under the rule open; the
//	                  default is STORAGE_QUOTA
//	STORAGE_CAPACITY  the largest sum of the quotas under the rule open
//	STORAGE_STUN      the STUN server, or "none"; unset asks the first
//	                  relay of ARC_RELAYS on port 3478
//	STORAGE_LOOPBACK  if set, use the loopback address too
func Run(ctx context.Context, opts provider.Options) error {
	quota, err := wire.ParseSize(os.Getenv("STORAGE_QUOTA"))
	if err != nil {
		return fmt.Errorf("STORAGE_QUOTA: %w", err)
	}
	if quota == 0 {
		quota = DefaultQuota
	}
	owners := strings.FieldsFunc(os.Getenv("STORAGE_OWNERS"), func(r rune) bool { return r == ',' || r == ' ' })
	for i, owner := range owners {
		owners[i] = strings.ToLower(owner)
		if !wire.IsHex64(owners[i]) {
			return fmt.Errorf("STORAGE_OWNERS: %q is not a public key of 64 hex digits", owner)
		}
	}
	r, err := admissionRules(quota)
	if err != nil {
		return err
	}
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	fmt.Fprintf(log, "the app admits accounts by the rule %s, and %d account keys of STORAGE_OWNERS\n", r.rule, len(owners))
	dir, err := Dir(os.Getenv("STORAGE_DIR"))
	if err != nil {
		return err
	}
	s := &server{
		store: &store{dir: dir, owners: owners, quota: quota, now: time.Now},
		rules: r,
		self:  strings.ToLower(os.Getenv("ARC_PUBLIC_KEY")),
		rtc: direct.Options{
			STUN:     direct.STUNURL(os.Getenv("STORAGE_STUN"), strings.Fields(os.Getenv("ARC_RELAYS"))),
			Loopback: os.Getenv("STORAGE_LOOPBACK") != "",
		},
		slots: make(chan struct{}, MaxSessions),
		log:   log,
	}
	defer func() { _ = s.store.close() }()
	s.sweep()
	n, err := s.store.openAll()
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "the app opened the index of %d accounts\n", n)
	return provider.Run(ctx, s, opts)
}

// admissionRules reads STORAGE_ADMIT, STORAGE_OPEN_QUOTA and
// STORAGE_CAPACITY.
func admissionRules(quota int64) (rules, error) {
	r := rules{rule: os.Getenv("STORAGE_ADMIT")}
	switch r.rule {
	case "":
		r.rule = RuleInvite
	case RuleInvite, RuleAsk:
	case RuleOpen:
		var err error
		if r.openQuota, err = wire.ParseSize(os.Getenv("STORAGE_OPEN_QUOTA")); err != nil {
			return r, fmt.Errorf("STORAGE_OPEN_QUOTA: %w", err)
		}
		if r.openQuota == 0 {
			r.openQuota = quota
		}
		if r.capacity, err = wire.ParseSize(os.Getenv("STORAGE_CAPACITY")); err != nil {
			return r, fmt.Errorf("STORAGE_CAPACITY: %w", err)
		}
		if r.capacity == 0 {
			return r, errors.New("STORAGE_ADMIT=open needs STORAGE_CAPACITY, the largest sum of the quotas, such as 100GiB")
		}
	default:
		return r, fmt.Errorf("STORAGE_ADMIT: %q is not invite, ask or open", r.rule)
	}
	return r, nil
}

// Dir is the directory of the blobs: dir, or ~/.local/state/arc-storage.
func Dir(dir string) (string, error) {
	if dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "arc-storage"), nil
}

// HandleRequest answers one call. A call with a grant set sets the grants
// of the account of its author. Another call opens a session: it checks
// the caller and the request, opens its end of the pipe, and returns the
// answer.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(request.Message), &fields) != nil {
		return "", errInvalidRequest
	}
	s.sweep()
	if _, ok := fields["close"]; ok {
		return s.close(request)
	}
	if _, ok := fields["grants"]; ok {
		return s.grant(request)
	}
	if _, ok := fields["join"]; ok {
		return s.join(request)
	}
	var req wire.Open
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 || req.Session.HoldMS < 0 ||
		time.Duration(req.Session.HoldMS)*time.Millisecond > direct.MaxHold ||
		req.Session.SDP.Type != webrtc.SDPTypeOffer || (req.Session.STUN != "" && !direct.IsIPSTUN(req.Session.STUN)) {
		return "", errInvalidRequest
	}
	account, err := s.accountOf(request.From)
	if err != nil {
		if errors.Is(err, errNoGrant) {
			return "", errNotOwner
		}
		fmt.Fprintf(s.log, "%.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return "", errBusy
	}
	return s.connect(account, req.Session, fmt.Sprintf("%.8s from %.8s", account, request.From))
}

// accountOf finds the account of the author of a call. An admitted
// account key uses its own account. A device key uses the admitted account
// whose newest grant set holds it. A closed account takes no session. The
// app reads the records from the disk on each call, so a change of the
// operator counts at once.
func (s *server) accountOf(author string) (string, error) {
	active, err := s.store.active(author)
	if err != nil || active {
		return author, err
	}
	names, err := s.store.names()
	if err != nil {
		return "", err
	}
	for _, name := range names {
		granted, err := s.store.granted(name, author)
		if err != nil {
			return "", err
		}
		if !granted {
			continue
		}
		if active, err := s.store.active(name); err != nil || active {
			return name, err
		}
	}
	return "", errNoGrant
}

// join answers a request for an account. The author is the account key.
// The reply is an account, admitted or pending, or the error refused, in
// the body of the reply.
func (s *server) join(request provider.Request) (string, error) {
	var req wire.JoinCall
	j := &req.Join
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 || j.Want < 0 || len(j.Note) > wire.MaxNote ||
		(j.Invite != "" && !wire.IsToken(j.Invite)) || (j.Citizen == "") != (j.Sig == "") {
		return "", errInvalidRequest
	}
	if j.Citizen != "" {
		if err := wire.VerifyJoin(j.Citizen, j.Sig, request.From, s.self); err != nil {
			fmt.Fprintf(s.log, "join of %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
			return "", errInvalidRequest
		}
	}
	reply, err := s.store.join(request.From, req.Join, s.rules)
	if err != nil {
		fmt.Fprintf(s.log, "join of %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	status := reply.Error
	if reply.Account != nil {
		status = reply.Account.Status
	}
	fmt.Fprintf(s.log, "join of %.8s: %s\n", request.From, status)
	raw, err := json.Marshal(reply)
	if err != nil {
		return "", errFailed
	}
	return string(raw), nil
}

// close ends the account of the author: the citizen leaves. Only an
// admitted account key closes its account, and only an account that holds
// no blob. The app ends each pipe of the account, and removes it at once.
func (s *server) close(request provider.Request) (string, error) {
	var req wire.CloseCall
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 {
		return "", errInvalidRequest
	}
	reply := wire.CloseReply{Version: 1, Closed: true}
	// A refused close leaves the pipes of the account open. The removal
	// checks again, because a session can put until its pipe ends.
	_, err := s.store.leaving(request.From)
	if err == nil {
		err = s.removing(request.From, func() error { return s.store.leave(request.From) })
	}
	if r, ok := errors.AsType[refusal](err); ok {
		reply = wire.CloseReply{Version: 1, Error: r.code, Message: r.message}
		err = nil
	}
	if errors.Is(err, errNoGrant) {
		return "", errNotOwner
	}
	if err != nil {
		fmt.Fprintf(s.log, "close of %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	if reply.Closed {
		fmt.Fprintf(s.log, "close of %.8s: the account is removed\n", request.From)
	} else {
		fmt.Fprintf(s.log, "close of %.8s: %s\n", request.From, terminal.EscapeLine(reply.Message))
	}
	raw, err := json.Marshal(reply)
	if err != nil {
		return "", errFailed
	}
	return string(raw), nil
}

// sweep removes the bytes of each closed account. It runs at the start of
// the service and at each call, so a closed account loses its bytes at the
// first call after its reads end.
func (s *server) sweep() {
	names, err := s.store.closed()
	if err != nil {
		fmt.Fprintf(s.log, "the sweep of closed accounts: %s\n", terminal.EscapeLine(err.Error()))
		return
	}
	for _, name := range names {
		var removed bool
		err := s.removing(name, func() (err error) {
			removed, err = s.store.removeClosed(name)
			return err
		})
		switch {
		case err != nil:
			fmt.Fprintf(s.log, "account %.8s is closed, and its removal failed: %s\n", name, terminal.EscapeLine(err.Error()))
		case removed:
			fmt.Fprintf(s.log, "account %.8s is closed: its bytes are removed\n", name)
		}
	}
}

// hold returns the lock of an account.
func (s *server) hold(account string) *sync.RWMutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == nil {
		s.holds = map[string]*sync.RWMutex{}
	}
	if s.holds[account] == nil {
		s.holds[account] = &sync.RWMutex{}
	}
	return s.holds[account]
}

// removing ends each pipe of an account, waits until no session holds
// the account, and then runs remove.
func (s *server) removing(account string, remove func() error) error {
	s.mu.Lock()
	var ends []func()
	for end := range s.pipes[account] {
		ends = append(ends, *end)
	}
	s.mu.Unlock()
	for _, end := range ends {
		end()
	}
	hold := s.hold(account)
	hold.Lock()
	defer hold.Unlock()
	return remove()
}

// track keeps the end of a pipe of an account until the pipe ends.
func (s *server) track(account string, end *func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pipes == nil {
		s.pipes = map[string]map[*func()]struct{}{}
	}
	if s.pipes[account] == nil {
		s.pipes[account] = map[*func()]struct{}{}
	}
	s.pipes[account][end] = struct{}{}
}

func (s *server) untrack(account string, end *func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pipes[account], end)
	if len(s.pipes[account]) == 0 {
		delete(s.pipes, account)
	}
}

// grant sets the grant set of the account of the author. Only an admitted
// account key sets grants.
func (s *server) grant(request provider.Request) (string, error) {
	var req wire.Grants
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 || req.At <= 0 || len(req.Devices) > wire.MaxGrants {
		return "", errInvalidRequest
	}
	for _, device := range req.Devices {
		if !wire.IsID(device) {
			return "", errInvalidRequest
		}
	}
	if active, err := s.store.active(request.From); err != nil || !active {
		if err != nil {
			fmt.Fprintf(s.log, "account %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
			return "", errFailed
		}
		return "", errNotOwner
	}
	if err := s.store.grant(request.From, req.Devices, req.At); err != nil {
		if r, ok := errors.AsType[refusal](err); ok {
			return "", provider.Error(r.code)
		}
		fmt.Fprintf(s.log, "account %.8s: %s\n", request.From, terminal.EscapeLine(err.Error()))
		return "", errFailed
	}
	fmt.Fprintf(s.log, "account %.8s: %d device keys have a grant\n", request.From, len(req.Devices))
	raw, err := json.Marshal(wire.Granted{Version: 1, Devices: len(req.Devices)})
	if err != nil {
		return "", errFailed
	}
	return string(raw), nil
}

// connect opens the end of the app for the WebRTC offer of a request, and
// returns the reply with the answer.
func (s *server) connect(account string, req wire.Session, label string) (string, error) {
	rtc := s.rtc
	rtc.PeerSTUN = req.STUN
	pc, err := direct.NewConnection(rtc)
	if err != nil {
		<-s.slots
		return "", errFailed
	}
	messages := make(chan webrtc.DataChannelMessage, 1024)
	done := make(chan struct{})
	// Close reports the closed state to the callback below, which calls end
	// again. A sync.Once would wait for itself there.
	var ended atomic.Bool
	var end func()
	end = func() {
		if ended.CompareAndSwap(false, true) {
			s.untrack(account, &end)
			close(done)
			<-s.slots
			_ = pc.Close()
		}
	}
	s.track(account, &end)
	hold := time.Duration(req.HoldMS) * time.Millisecond
	// An end that never connects must not keep its slot.
	unopened := time.AfterFunc(direct.OpenWait+hold, end)
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		switch state {
		case webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			end()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		// The client closes the data channel at the end of its session. The
		// slot is then free at once, not after the ICE timeouts.
		dc.OnClose(end)
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			select {
			case messages <- m:
			case <-done:
			}
		})
		dc.OnOpen(func() {
			unopened.Stop()
			fmt.Fprintf(s.log, "%s: a session on %s\n", label, direct.SelectedPath(pc))
			go func() {
				defer end()
				// A removal of the account waits for the session. A
				// session that starts after the removal finds no account.
				hold := s.hold(account)
				hold.RLock()
				defer hold.RUnlock()
				if active, err := s.store.active(account); err != nil || !active {
					return
				}
				sess := &session{store: s.store, account: account, dc: dc, messages: messages, done: done}
				if err := sess.serve(); err != nil {
					fmt.Fprintf(s.log, "%s: %s\n", label, terminal.EscapeLine(err.Error()))
				}
			}()
		})
	})

	remote := req.SDP
	var held []webrtc.ICECandidateInit
	if hold > 0 {
		held = direct.HoldCandidates(&remote)
	}
	if err := pc.SetRemoteDescription(remote); err != nil {
		end()
		return "", errInvalidRequest
	}
	if hold > 0 {
		time.AfterFunc(hold, func() {
			for _, candidate := range held {
				_ = pc.AddICECandidate(candidate)
			}
		})
	}
	local, err := pc.CreateAnswer(nil)
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		end()
		return "", errFailed
	}
	raw, err := json.Marshal(wire.Answer{Version: 1, SDP: local})
	if err != nil {
		end()
		return "", errFailed
	}
	return string(raw), nil
}

// session serves the requests of one pipe. It reads the messages in
// order, and answers each request in order. A client with FeatureBatch
// keeps several requests in flight, and each reply carries the N of its
// request.
//
// A put gets the reply send, and waits in sends. The client sends the
// bytes of each put in the order of the send replies, so each binary run
// belongs to the oldest put in sends. When a run is complete, its file is
// written, and the put waits in group. The session writes the entries of
// the group in one transaction when no message waits, before a request
// that is not a put, or when the window is full. Only then does each put
// get the reply stored.
type session struct {
	store    *store
	account  string
	dc       *webrtc.DataChannel
	messages <-chan webrtc.DataChannelMessage
	done     <-chan struct{}

	sends []sending
	group []written
}

// sending is a put that got the reply send, and waits for its bytes.
type sending struct {
	n    uint64
	id   string
	size int64
	tag  []byte
	// file takes the bytes as they come; got counts them.
	file *blobFile
	got  int64
}

// errClosed says that the client closed the pipe.
var errClosed = errors.New("closed")

func (s *session) next() (webrtc.DataChannelMessage, error) {
	select {
	case m := <-s.messages:
		return m, nil
	case <-s.done:
		return webrtc.DataChannelMessage{}, errClosed
	case <-time.After(IdleWait):
		return webrtc.DataChannelMessage{}, errors.New("the client sent nothing for 30 s")
	}
}

func (s *session) serve() error {
	// A put whose bytes did not all come leaves no file.
	defer func() {
		for _, p := range s.sends {
			if p.file != nil {
				p.file.abort()
			}
		}
	}()
	for {
		if len(s.group) > 0 && len(s.messages) == 0 {
			if err := s.commit(); err != nil {
				return err
			}
		}
		m, err := s.next()
		if errors.Is(err, errClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		if !m.IsString {
			if err := s.bytes(m.Data); err != nil {
				return err
			}
			continue
		}
		var req wire.Request
		if json.Unmarshal(m.Data, &req) != nil {
			return errors.New("the client sent a message that is not a request")
		}
		if req.Op != wire.OpPut && len(s.group) > 0 {
			if err := s.commit(); err != nil {
				return err
			}
		}
		if err := s.handle(req); err != nil {
			return err
		}
	}
}

// handle answers one request. It returns an error only if the pipe cannot
// go on.
func (s *session) handle(req wire.Request) error {
	switch req.Op {
	case wire.OpHello:
		return s.reply(wire.Reply{N: req.N, Version: 2, Features: []string{wire.FeatureBatch, wire.FeaturePieces, wire.FeatureAudit, wire.FeatureDrain}, Window: wire.Window})
	case wire.OpDrain:
		r, err := s.store.drain(s.account, req.Off)
		if err != nil {
			return s.refuse(req.N, err)
		}
		r.N = req.N
		return s.reply(r)
	case wire.OpList:
		limit := req.Limit
		if limit <= 0 || limit > wire.MaxList {
			limit = wire.MaxList
		}
		entries, next, err := s.store.list(s.account, req.After, limit)
		if err != nil {
			return s.refuse(req.N, err)
		}
		return s.reply(wire.Reply{N: req.N, Entries: entries, Next: next})
	case wire.OpUsage:
		r, err := s.store.usage(s.account)
		if err != nil {
			return s.refuse(req.N, err)
		}
		r.N = req.N
		return s.reply(r)
	}
	ids, ok := idsOf(req)
	if !ok {
		return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: fmt.Sprintf("a request names one blob ID, or 1 to %d blob IDs; a blob ID is 64 lower-case hex digits", wire.MaxBatch)})
	}
	switch req.Op {
	case wire.OpPut:
		if req.IDs != nil {
			return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: "a put names one blob ID"})
		}
		return s.put(req)
	case wire.OpGet:
		return s.get(req, ids)
	case wire.OpAudit:
		if !wire.IsNonce(req.Nonce) {
			return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: fmt.Sprintf("an audit has a nonce of %d bytes in lower-case hex", wire.NonceSize)})
		}
		nonce, _ := hex.DecodeString(req.Nonce)
		items, err := s.store.audit(s.account, ids, nonce)
		if err != nil {
			return s.refuse(req.N, err)
		}
		if req.IDs != nil {
			return s.reply(wire.Reply{N: req.N, Items: items})
		}
		if items[0].Error != "" {
			return s.reply(wire.Reply{N: req.N, Error: items[0].Error, Message: "the account holds no blob " + req.ID})
		}
		return s.reply(wire.Reply{N: req.N, Size: items[0].Size, Proof: items[0].Proof})
	case wire.OpDelete:
		items, err := s.store.remove(s.account, ids)
		if err != nil {
			return s.refuse(req.N, err)
		}
		if req.IDs != nil {
			return s.reply(wire.Reply{N: req.N, Items: items})
		}
		return s.reply(wire.Reply{N: req.N, Status: items[0].Status, Seq: items[0].Seq})
	}
	return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: "unknown operation " + strconv.Quote(req.Op)})
}

// idsOf returns the blob IDs of a request: its ID, or its IDs.
func idsOf(req wire.Request) ([]string, bool) {
	if req.IDs == nil {
		return []string{req.ID}, wire.IsID(req.ID)
	}
	if req.ID != "" || len(req.IDs) == 0 || len(req.IDs) > wire.MaxBatch {
		return nil, false
	}
	for _, id := range req.IDs {
		if !wire.IsID(id) {
			return nil, false
		}
	}
	return req.IDs, true
}

// get answers a get. With IDs, the reply gives each size, and the bytes of
// each found blob follow, in order.
func (s *session) get(req wire.Request, ids []string) error {
	if req.IDs == nil {
		data, err := s.store.get(s.account, req.ID)
		if err != nil {
			return s.refuse(req.N, err)
		}
		if err := s.reply(wire.Reply{N: req.N, Size: int64(len(data))}); err != nil {
			return err
		}
		return direct.Send(s.dc, bytes.NewReader(data), int64(len(data)))
	}
	items, err := s.store.sizes(s.account, ids)
	if err != nil {
		return s.refuse(req.N, err)
	}
	if err := s.reply(wire.Reply{N: req.N, Items: items}); err != nil {
		return err
	}
	for _, item := range items {
		if item.Error != "" {
			continue
		}
		// The reply named the size, so a file that does not match it ends
		// the pipe.
		data, err := os.ReadFile(s.store.blobPath(s.account, item.ID))
		if err != nil {
			return err
		}
		if int64(len(data)) != item.Size {
			return fmt.Errorf("the file of %s has %d bytes, and its entry %d", item.ID, len(data), item.Size)
		}
		if err := direct.Send(s.dc, bytes.NewReader(data), item.Size); err != nil {
			return err
		}
	}
	return nil
}

// put admits a blob. If its bytes can go, the put waits in sends.
func (s *session) put(req wire.Request) error {
	if len(s.sends)+len(s.group) >= wire.Window {
		return fmt.Errorf("the client has more than %d puts in flight", wire.Window)
	}
	var reserved int64
	for _, p := range s.sends {
		if p.id == req.ID {
			return s.reply(wire.Reply{N: req.N, Status: wire.StatusHeld})
		}
		reserved += p.size
	}
	for _, p := range s.group {
		if p.id == req.ID {
			return s.reply(wire.Reply{N: req.N, Status: wire.StatusHeld})
		}
		reserved += p.size
	}
	var tag []byte
	if req.Tag != "" {
		var err error
		if tag, err = base64.StdEncoding.DecodeString(req.Tag); err != nil || len(tag) != wire.TagSize {
			return s.reply(wire.Reply{N: req.N, Error: wire.ErrInvalidRequest, Message: fmt.Sprintf("the tag of a piece is %d bytes in standard base64", wire.TagSize)})
		}
	}
	status, err := s.store.admit(s.account, req.ID, req.Size, reserved, tag != nil)
	if err != nil {
		return s.refuse(req.N, err)
	}
	if status == wire.StatusSend {
		s.sends = append(s.sends, sending{n: req.N, id: req.ID, size: req.Size, tag: tag})
	}
	return s.reply(wire.Reply{N: req.N, Status: status})
}

// bytes takes one binary message for the oldest put in sends. When the
// bytes of the put are complete, it writes its file.
func (s *session) bytes(data []byte) error {
	if len(s.sends) == 0 {
		return errors.New("the client sent bytes for no put")
	}
	p := &s.sends[0]
	if p.got+int64(len(data)) > p.size {
		return errors.New("the client sent other bytes than the size of the put")
	}
	if p.file == nil {
		f, err := s.store.createBlob(s.account, p.id)
		if err != nil {
			return err
		}
		p.file = f
	}
	if err := p.file.write(data); err != nil {
		return err
	}
	p.got += int64(len(data))
	if p.got < p.size {
		return nil
	}
	head := s.sends[0]
	s.sends = s.sends[1:]
	if err := head.file.finish(); err != nil {
		return s.refuse(head.n, err)
	}
	s.group = append(s.group, written{n: head.n, id: head.id, size: head.size, tag: head.tag})
	if len(s.group) >= wire.Window {
		return s.commit()
	}
	return nil
}

// commit writes the entries of the group, and answers each put of it.
func (s *session) commit() error {
	group := s.group
	s.group = nil
	results, err := s.store.commit(s.account, group)
	if err != nil {
		return s.refuse(group[0].n, err)
	}
	for i, r := range results {
		var err error
		if r.err != nil {
			err = s.refuse(group[i].n, r.err)
		} else {
			err = s.reply(wire.Reply{N: group[i].n, Status: r.status, Seq: r.seq})
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// refuse answers with the code of a refusal of the store. Another error
// is a failure of this machine; the reply does not show it.
func (s *session) refuse(n uint64, err error) error {
	if r, ok := errors.AsType[refusal](err); ok {
		return s.reply(wire.Reply{N: n, Error: r.code, Message: r.message})
	}
	_ = s.reply(wire.Reply{N: n, Error: wire.ErrFailed, Message: "the storage citizen could not do it"})
	return err
}

func (s *session) reply(r wire.Reply) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.dc.SendText(string(raw))
}
