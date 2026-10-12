// Package server holds encrypted blobs for the owners of a storage
// citizen. It cannot read them.
//
// An owner is an account key: a key that the signer of a citizen derives
// for this one storage citizen. The account key asks for an account with
// wire.JoinCall, and the rule of admission decides. It then sends
// wire.Grants, the device keys that can use the account. A request is one ARC call,
// wire.Open, with a WebRTC offer, from the account key or from a device
// key with a grant. The reply is wire.Answer, with the
// WebRTC answer. The two programs then open one pipe of sdk/direct, and
// the client sends put, list, get, delete and usage requests on it. No blob
// goes through ARC.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	wire "github.com/gezibash/arc/sdk/storage"
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
		fmt.Fprintf(s.log, "%.8s: %v\n", request.From, err)
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
			fmt.Fprintf(s.log, "join of %.8s: %v\n", request.From, err)
			return "", errInvalidRequest
		}
	}
	reply, err := s.store.join(request.From, req.Join, s.rules)
	if err != nil {
		fmt.Fprintf(s.log, "join of %.8s: %v\n", request.From, err)
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
			fmt.Fprintf(s.log, "account %.8s: %v\n", request.From, err)
			return "", errFailed
		}
		return "", errNotOwner
	}
	if err := s.store.grant(request.From, req.Devices, req.At); err != nil {
		if r, ok := errors.AsType[refusal](err); ok {
			return "", provider.Error(r.code)
		}
		fmt.Fprintf(s.log, "account %.8s: %v\n", request.From, err)
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
	end := func() {
		if ended.CompareAndSwap(false, true) {
			close(done)
			<-s.slots
			_ = pc.Close()
		}
	}
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
				sess := &session{store: s.store, account: account, dc: dc, messages: messages, done: done}
				if err := sess.serve(); err != nil {
					fmt.Fprintf(s.log, "%s: %v\n", label, err)
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

// session serves the requests of one pipe, one at a time.
type session struct {
	store    *store
	account  string
	dc       *webrtc.DataChannel
	messages <-chan webrtc.DataChannelMessage
	done     <-chan struct{}
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
	for {
		m, err := s.next()
		if errors.Is(err, errClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		var req wire.Request
		if !m.IsString || json.Unmarshal(m.Data, &req) != nil {
			return errors.New("the client sent a message that is not a request")
		}
		if err := s.handle(req); err != nil {
			return err
		}
	}
}

// handle answers one request. It returns an error only if the pipe cannot
// go on.
func (s *session) handle(req wire.Request) error {
	if req.Op != wire.OpList && req.Op != wire.OpUsage && !wire.IsID(req.ID) {
		return s.reply(wire.Reply{Error: wire.ErrInvalidRequest, Message: "a blob ID is 64 lower-case hex digits"})
	}
	switch req.Op {
	case wire.OpPut:
		return s.put(req)
	case wire.OpGet:
		data, err := s.store.get(s.account, req.ID)
		if err != nil {
			return s.refuse(err)
		}
		if err := s.reply(wire.Reply{Size: int64(len(data))}); err != nil {
			return err
		}
		return direct.Send(s.dc, bytes.NewReader(data), int64(len(data)))
	case wire.OpDelete:
		status, seq, err := s.store.remove(s.account, req.ID)
		if err != nil {
			return s.refuse(err)
		}
		return s.reply(wire.Reply{Status: status, Seq: seq})
	case wire.OpList:
		limit := req.Limit
		if limit <= 0 || limit > wire.MaxList {
			limit = wire.MaxList
		}
		entries, next, err := s.store.list(s.account, req.After, limit)
		if err != nil {
			return s.refuse(err)
		}
		return s.reply(wire.Reply{Entries: entries, Next: next})
	case wire.OpUsage:
		r, err := s.store.usage(s.account)
		if err != nil {
			return s.refuse(err)
		}
		return s.reply(r)
	}
	return s.reply(wire.Reply{Error: wire.ErrInvalidRequest, Message: "unknown operation " + strconv.Quote(req.Op)})
}

// put admits a blob, takes its bytes, and stores it.
func (s *session) put(req wire.Request) error {
	status, err := s.store.admit(s.account, req.ID, req.Size)
	if err != nil {
		return s.refuse(err)
	}
	if err := s.reply(wire.Reply{Status: status}); err != nil || status != wire.StatusSend {
		return err
	}
	data := make([]byte, 0, req.Size)
	for int64(len(data)) < req.Size {
		m, err := s.next()
		if err != nil {
			return err
		}
		if m.IsString || int64(len(data)+len(m.Data)) > req.Size {
			return errors.New("the client sent other bytes than the size of the put")
		}
		data = append(data, m.Data...)
	}
	status, seq, err := s.store.put(s.account, req.ID, data)
	if err != nil {
		return s.refuse(err)
	}
	return s.reply(wire.Reply{Status: status, Seq: seq})
}

// refuse answers with the code of a refusal of the store. Another error
// is a failure of this machine; the reply does not show it.
func (s *session) refuse(err error) error {
	if r, ok := errors.AsType[refusal](err); ok {
		return s.reply(wire.Reply{Error: r.code, Message: r.message})
	}
	_ = s.reply(wire.Reply{Error: wire.ErrFailed, Message: "the storage citizen could not do it"})
	return err
}

func (s *session) reply(r wire.Reply) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return s.dc.SendText(string(raw))
}
