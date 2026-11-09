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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
