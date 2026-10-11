// Package server holds encrypted blobs for the owners of a storage
// citizen. It cannot read them.
//
// A request is one ARC call, wire.Open: the pseudonym of the owner for this
// storage citizen, and a WebRTC offer. The reply is wire.Answer, with the
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
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/apps/storage/wire"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
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
	store  *store
	owners []string
	rtc    direct.Options
	slots  chan struct{}
	log    io.Writer
}

// Run serves the storage app through core ARC. The environment gives the
// settings:
//
//	STORAGE_DIR       the directory of the blobs
//	STORAGE_OWNERS    the public keys, in hex, that can open an account,
//	                  separated by spaces or commas
//	STORAGE_QUOTA     the quota of each account, such as 512MiB or 1GiB
//	STORAGE_STUN      the STUN server, or "none"; unset asks the first
//	                  relay of ARC_RELAYS on port 3478
//	STORAGE_LOOPBACK  if set, use the loopback address too
func Run(ctx context.Context, opts provider.Options) error {
	quota, err := ParseSize(os.Getenv("STORAGE_QUOTA"))
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
	log := opts.Log
	if log == nil {
		log = io.Discard
	}
	if len(owners) == 0 {
		fmt.Fprintln(log, "STORAGE_OWNERS is not set, so the app refuses each caller")
	}
	dir, err := Dir(os.Getenv("STORAGE_DIR"))
	if err != nil {
		return err
	}
	s := &server{
		store:  &store{dir: dir, quota: quota, now: time.Now},
		owners: owners,
		rtc: direct.Options{
			STUN:     direct.STUNURL(os.Getenv("STORAGE_STUN"), strings.Fields(os.Getenv("ARC_RELAYS"))),
			Loopback: os.Getenv("STORAGE_LOOPBACK") != "",
		},
		slots: make(chan struct{}, MaxSessions),
		log:   log,
	}
	return provider.Run(ctx, s, opts)
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

// ParseSize reads a number of bytes, such as 4096, 64KiB, 512MiB or 1GiB.
// An empty value is 0.
func ParseSize(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	number, shift := value, 0
	for suffix, s := range map[string]int{"KiB": 10, "MiB": 20, "GiB": 30, "TiB": 40} {
		if rest, ok := strings.CutSuffix(value, suffix); ok {
			number, shift = rest, s
		}
	}
	n, err := strconv.ParseInt(number, 10, 64)
	if err != nil || n < 0 || n > (1<<62)>>shift {
		return 0, fmt.Errorf("%q is not a size such as 4096, 64KiB, 512MiB or 1GiB", value)
	}
	return n << shift, nil
}

// HandleRequest answers one call: it checks the caller and the request,
// opens its end of the pipe, and returns the answer.
func (s *server) HandleRequest(_ context.Context, request provider.Request) (string, error) {
	var req wire.Open
	if json.Unmarshal([]byte(request.Message), &req) != nil || req.Version != 1 ||
		!wire.IsAccount(req.Account) || req.Session.HoldMS < 0 ||
		time.Duration(req.Session.HoldMS)*time.Millisecond > direct.MaxHold ||
		req.Session.SDP.Type != webrtc.SDPTypeOffer || (req.Session.STUN != "" && !direct.IsIPSTUN(req.Session.STUN)) {
		return "", errInvalidRequest
	}
	// The owner check. A later step also takes a device key with a grant
	// that the account key signed; see docs/proposals/storage.md, section
	// 2.3.
	if !slices.Contains(s.owners, request.From) {
		return "", errNotOwner
	}
	if err := s.store.authorize(req.Account, request.From); err != nil {
		if errors.Is(err, errNoGrant) {
			return "", errNotOwner
		}
		fmt.Fprintf(s.log, "account %s: %v\n", req.Account, err)
		return "", errFailed
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return "", errBusy
	}
	return s.connect(req.Account, req.Session, fmt.Sprintf("%.8s from %.8s", req.Account, request.From))
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
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			end()
		}
	})
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
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
		used, blobs, err := s.store.usage(s.account)
		if err != nil {
			return s.refuse(err)
		}
		return s.reply(wire.Reply{Used: used, Quota: s.store.quota, Blobs: blobs})
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
