package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// HoldFirst asks the storage citizen to wait in the first attempt, not
	// in the second.
	HoldFirst bool
	// NoRelay stops after the direct attempts.
	NoRelay bool
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
}

// Open opens a pipe. need is the number of bytes that the session sends or
// gets; the relay attempt checks it against the TURN quota.
func (c Client) Open(ctx context.Context, need int64) (*Conn, error) {
	if c.Notes == nil {
		c.Notes = io.Discard
	}
	var opened *Conn
	err := direct.Attempts{
		Options: c.Options, HoldFirst: c.HoldFirst, NoRelay: c.NoRelay, TURN: c.TURN, Notes: c.Notes,
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
	case <-time.After(direct.OpenWait + hold):
		return nil, direct.ErrNoPath
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	s.Path = direct.SelectedPath(pc)
	return s, nil
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

// IsRefused says if err is a refusal with the code.
func IsRefused(err error, code string) bool {
	var r *Refused
	return errors.As(err, &r) && r.Code == code
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
	raw, err := json.Marshal(req)
	if err != nil {
		return Reply{}, err
	}
	if err := s.dc.SendText(string(raw)); err != nil {
		return Reply{}, err
	}
	return s.reply(ctx)
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

// Put gives one blob. The size of blob must be a power of two, at least
// MinBlob: see Pad. The status is StatusStored,
// StatusHeld or StatusTombstoned.
func (s *Conn) Put(ctx context.Context, id string, blob []byte) (string, error) {
	r, err := s.do(ctx, Request{Op: OpPut, ID: id, Size: int64(len(blob))})
	if err != nil || r.Status != StatusSend {
		return r.Status, err
	}
	if err := direct.Send(s.dc, bytes.NewReader(blob), int64(len(blob))); err != nil {
		return "", err
	}
	r, err = s.reply(ctx)
	return r.Status, err
}

// List returns the entries after a cursor, and the next cursor. It reads
// each page until the end.
func (s *Conn) List(ctx context.Context, after uint64) ([]Entry, uint64, error) {
	var all []Entry
	for {
		r, err := s.do(ctx, Request{Op: OpList, After: after, Limit: MaxList})
		if err != nil {
			return all, after, err
		}
		all = append(all, r.Entries...)
		if len(r.Entries) == 0 || r.Next <= after {
			return all, after, nil
		}
		after = r.Next
	}
}

// Get returns the bytes of one blob.
func (s *Conn) Get(ctx context.Context, id string) ([]byte, error) {
	r, err := s.do(ctx, Request{Op: OpGet, ID: id})
	if err != nil {
		return nil, err
	}
	if !GoodSize(r.Size) {
		return nil, fmt.Errorf("the storage citizen announced a blob of %d bytes", r.Size)
	}
	data := make([]byte, 0, r.Size)
	for int64(len(data)) < r.Size {
		m, err := s.next(ctx)
		if err != nil {
			return nil, err
		}
		if m.IsString || int64(len(data)+len(m.Data)) > r.Size {
			return nil, errors.New("the storage citizen sent other bytes than the size of the blob")
		}
		data = append(data, m.Data...)
	}
	return data, nil
}

// Delete removes one blob. The status is StatusDeleted or
// StatusAbsent.
func (s *Conn) Delete(ctx context.Context, id string) (string, error) {
	r, err := s.do(ctx, Request{Op: OpDelete, ID: id})
	return r.Status, err
}

// Usage is the result of a usage request.
type Usage struct {
	Used, Quota int64
	Blobs       int
	// State, Plan and Expires are empty from an older storage app.
	State, Plan string
	Expires     int64
}

// Usage returns the bytes in use, the quota of the account, the number of
// blobs, and the record of the account.
func (s *Conn) Usage(ctx context.Context) (Usage, error) {
	r, err := s.do(ctx, Request{Op: OpUsage})
	return Usage{Used: r.Used, Quota: r.Quota, Blobs: r.Blobs, State: r.State, Plan: r.Plan, Expires: r.Expires}, err
}
