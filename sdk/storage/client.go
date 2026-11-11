package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/pion/webrtc/v4"
)

// Client opens sessions with one storage citizen.
type Client struct {
	// Call sends the ARC call that opens a session: it gives session, the
	// JSON of a Session, to the manifest command "open" of the installed
	// storage app, and returns the reply body. The command adds the
	// pseudonym of the account, and ARC signs the call.
	Call func(ctx context.Context, session string) ([]byte, error)
	// TURN gets a credential for the TURN server of the relay.
	TURN func(ctx context.Context) (direct.Credential, error)
	// Options are the settings of the connection.
	Options direct.Options
	// AnswererFirst lets the storage citizen send first in the first
	// attempt, and asks it to wait in the second.
	AnswererFirst bool
	// NoRelay stops after the direct attempts.
	NoRelay bool
	// OnlyRelay skips the direct attempts: the pipe goes through the TURN
	// server of the relay.
	OnlyRelay bool
	// Notes gets one line for each event that a person wants to know.
	Notes io.Writer
}

// Conn is one open pipe to the storage app.
type Conn struct {
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	messages chan webrtc.DataChannelMessage
	closed   chan struct{}
	// Path names the kinds of the two addresses of the connection.
	Path string
	// window is the count of requests that the app takes in flight, from
	// hello: 0 before hello, 1 for an older app.
	window int
	// features are the features of the reply to hello.
	features []string
	// n is the N of the last numbered request.
	n uint64
}

// Open opens a pipe. need is the number of bytes that the session sends or
// gets; the relay attempt checks it against the TURN quota.
func (c Client) Open(ctx context.Context, need int64) (*Conn, error) {
	if c.Notes == nil {
		c.Notes = io.Discard
	}
	var opened *Conn
	err := direct.Attempts{
		Options: c.Options, AnswererFirst: c.AnswererFirst, NoRelay: c.NoRelay, OnlyRelay: c.OnlyRelay, TURN: c.TURN, Notes: c.Notes,
		Need: func() int64 { return need },
	}.Run(ctx, func(ctx context.Context, options direct.Options, hold time.Duration) (err error) {
		opened, err = c.attempt(ctx, options, hold)
		return err
	})
	return opened, err
}

// attempt opens one connection. It returns direct.ErrNoPath if the data
// channel did not open.
func (c Client) attempt(ctx context.Context, options direct.Options, hold time.Duration) (_ *Conn, err error) {
	pc, err := direct.NewConnection(options)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			_ = pc.Close()
		}
	}()
	dc, err := pc.CreateDataChannel("storage", nil)
	if err != nil {
		return nil, err
	}
	s, opened := newConn(pc, dc)
	// ICE fails after about 8 s with no path. The attempt then ends at
	// once, not after OpenWait.
	failed := make(chan struct{})
	var failOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			failOnce.Do(func() { close(failed) })
		}
	})

	local, err := pc.CreateOffer(nil)
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		return nil, err
	}
	stun, err := direct.OfferSTUN(ctx, options, local)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(Session{HoldMS: int(hold / time.Millisecond), STUN: stun, SDP: local})
	if err != nil {
		return nil, err
	}
	reply, err := c.Call(ctx, string(body))
	if err != nil {
		return nil, err
	}
	var remote Answer
	if json.Unmarshal(reply, &remote) != nil || remote.Version != 1 {
		return nil, fmt.Errorf("the storage citizen gave no answer: %.200q", reply)
	}
	if err := pc.SetRemoteDescription(remote.SDP); err != nil {
		return nil, err
	}
	select {
	case <-opened:
	case <-failed:
		return nil, direct.ErrNoPath
	case <-time.After(direct.OpenWait + hold):
		return nil, direct.ErrNoPath
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.Path = direct.SelectedPath(pc)
	return s, nil
}

// newConn makes the Conn of a data channel. opened closes when the data
// channel opens.
func newConn(pc *webrtc.PeerConnection, dc *webrtc.DataChannel) (*Conn, chan struct{}) {
	s := &Conn{pc: pc, dc: dc, messages: make(chan webrtc.DataChannelMessage, 1024), closed: make(chan struct{})}
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	dc.OnClose(func() { close(s.closed) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case s.messages <- m:
		case <-s.closed:
		}
	})
	return s, opened
}

// Close closes the data channel, so that the storage citizen frees the
// slot of the session at once, and then closes the connection.
func (s *Conn) Close() {
	if s.dc.Close() == nil {
		select {
		case <-s.closed:
		case <-time.After(time.Second):
		}
	}
	_ = s.pc.Close()
}

// Refused is a refusal of the storage citizen.
type Refused struct{ Code, Message string }

func (r *Refused) Error() string { return r.Message }

// IsRefused says if err is a refusal with the code. An empty code
// matches each refusal.
func IsRefused(err error, code string) bool {
	var r *Refused
	return errors.As(err, &r) && (code == "" || r.Code == code)
}

func (s *Conn) next(ctx context.Context) (webrtc.DataChannelMessage, error) {
	select {
	case m := <-s.messages:
		return m, nil
	case <-s.closed:
		return webrtc.DataChannelMessage{}, errors.New("the storage citizen closed the pipe")
	case <-time.After(60 * time.Second):
		return webrtc.DataChannelMessage{}, errors.New("the storage citizen did not answer in 60 s")
	case <-ctx.Done():
		return webrtc.DataChannelMessage{}, ctx.Err()
	}
}

// do sends a request, and reads its reply.
func (s *Conn) do(ctx context.Context, req Request) (Reply, error) {
	if err := s.send(req); err != nil {
		return Reply{}, err
	}
	return s.reply(ctx)
}

func (s *Conn) send(req Request) error {
	raw, err := json.Marshal(req)
	if err != nil {
		return err
	}
	return s.dc.SendText(string(raw))
}

func (s *Conn) reply(ctx context.Context) (Reply, error) {
	m, err := s.next(ctx)
	if err != nil {
		return Reply{}, err
	}
	var r Reply
	if !m.IsString || json.Unmarshal(m.Data, &r) != nil {
		return r, errors.New("the storage citizen sent a message that is not a reply")
	}
	if r.Error != "" {
		return r, &Refused{Code: r.Error, Message: r.Message}
	}
	return r, nil
}
