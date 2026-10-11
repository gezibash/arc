// Package client opens a pipe to the storage app of a storage citizen,
// and sends put, list, get, delete and usage requests on it. It makes the
// ARC call with the arc program, so ARC signs the call with the key of
// this citizen, and the manifest command "open" adds the pseudonym of the
// account.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/gezibash/arc/apps/storage/wire"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/pion/webrtc/v4"
)

// Arc names the arc program and the arc home of this citizen.
type Arc struct {
	// Program is the arc program. The default is "arc" on PATH.
	Program string
	// Home is the arc home. The default is the home that arc picks.
	Home string
}

// Command makes a command of the arc program, in the arc home.
func (a Arc) Command(ctx context.Context, args ...string) *exec.Cmd {
	program := a.Program
	if program == "" {
		program = "arc"
	}
	if a.Home != "" {
		args = append([]string{"--home", a.Home}, args...)
	}
	return exec.CommandContext(ctx, program, args...)
}

// Relays asks arc for the relay URLs of this citizen. It leaves out the
// indexers.
func (a Arc) Relays(ctx context.Context) ([]string, error) {
	out, err := a.Command(ctx, "relay", "ls").Output()
	if err != nil {
		return nil, fmt.Errorf("arc relay ls: %w", err)
	}
	var relays []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 && (strings.HasPrefix(fields[0], "ws://") || strings.HasPrefix(fields[0], "wss://")) {
			relays = append(relays, fields[0])
		}
	}
	return relays, nil
}

// TURN asks the first relay of this citizen for a TURN credential.
func (a Arc) TURN(ctx context.Context) (direct.Credential, error) {
	return direct.RelayTURN(a.Command(ctx, "relay", "turn"))
}

// Client opens sessions with one storage citizen.
type Client struct {
	Arc Arc
	// App is the installed name of the storage app of the storage citizen.
	// The default is "storage".
	App string
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

// Session is one open pipe to the storage app.
type Session struct {
	pc       *webrtc.PeerConnection
	dc       *webrtc.DataChannel
	messages chan webrtc.DataChannelMessage
	closed   chan struct{}
	// Path names the kinds of the two addresses of the connection.
	Path string
}

// Open opens a pipe. need is the number of bytes that the session sends or
// gets; the relay attempt checks it against the TURN quota.
func (c Client) Open(ctx context.Context, need int64) (*Session, error) {
	if c.Notes == nil {
		c.Notes = io.Discard
	}
	var opened *Session
	err := direct.Attempts{
		Options: c.Options, HoldFirst: c.HoldFirst, NoRelay: c.NoRelay, TURN: c.Arc.TURN, Notes: c.Notes,
		Need: func() int64 { return need },
	}.Run(ctx, func(ctx context.Context, options direct.Options, hold time.Duration) (err error) {
		opened, err = c.attempt(ctx, options, hold)
		return err
	})
	return opened, err
}

// attempt opens one connection. It returns direct.ErrNoPath if the data
// channel did not open.
func (c Client) attempt(ctx context.Context, options direct.Options, hold time.Duration) (_ *Session, err error) {
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
	s := &Session{pc: pc, dc: dc, messages: make(chan webrtc.DataChannelMessage, 1024), closed: make(chan struct{})}
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
	body, err := json.Marshal(wire.Session{HoldMS: int(hold / time.Millisecond), STUN: stun, SDP: local})
	if err != nil {
		return nil, err
	}
	reply, err := c.call(ctx, string(body))
	if err != nil {
		return nil, err
	}
	var remote wire.Answer
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

// call runs the manifest command "open" of the installed app. The command
// adds the pseudonym of the account, and makes the ARC call.
func (c Client) call(ctx context.Context, session string) ([]byte, error) {
	app := c.App
	if app == "" {
		app = "storage"
	}
	command := c.Arc.Command(ctx, app, "open", session)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	reply, err := command.Output()
	if err == nil {
		return bytes.TrimSpace(reply), nil
	}
	text := strings.TrimSpace(stderr.String())
	switch {
	case strings.Contains(text, "not_owner"):
		return nil, errors.New("the storage citizen refused this citizen: it is not an owner of the account")
	case strings.Contains(text, "busy"):
		return nil, errors.New("the storage citizen runs too many sessions now; try again later")
	}
	return nil, fmt.Errorf("the ARC call failed: %s\nif the storage app is not installed, run: arc install <key of the storage citizen> storage", text)
}

// Close closes the pipe.
func (s *Session) Close() { _ = s.pc.Close() }

// Refused is a refusal of the storage citizen.
type Refused struct{ Code, Message string }

func (r *Refused) Error() string { return r.Message }

// IsRefused says if err is a refusal with the code.
func IsRefused(err error, code string) bool {
	var r *Refused
	return errors.As(err, &r) && r.Code == code
}

func (s *Session) next(ctx context.Context) (webrtc.DataChannelMessage, error) {
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
func (s *Session) do(ctx context.Context, req wire.Request) (wire.Reply, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return wire.Reply{}, err
	}
	if err := s.dc.SendText(string(raw)); err != nil {
		return wire.Reply{}, err
	}
	return s.reply(ctx)
}

func (s *Session) reply(ctx context.Context) (wire.Reply, error) {
	m, err := s.next(ctx)
	if err != nil {
		return wire.Reply{}, err
	}
	var r wire.Reply
	if !m.IsString || json.Unmarshal(m.Data, &r) != nil {
		return r, errors.New("the storage citizen sent a message that is not a reply")
	}
	if r.Error != "" {
		return r, &Refused{Code: r.Error, Message: r.Message}
	}
	return r, nil
}

// Put gives one blob. The size of blob must be a power of two, at least
// wire.MinBlob: see wire.Pad. The status is wire.StatusStored,
// wire.StatusHeld or wire.StatusTombstoned.
func (s *Session) Put(ctx context.Context, id string, blob []byte) (string, error) {
	r, err := s.do(ctx, wire.Request{Op: wire.OpPut, ID: id, Size: int64(len(blob))})
	if err != nil || r.Status != wire.StatusSend {
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
func (s *Session) List(ctx context.Context, after uint64) ([]wire.Entry, uint64, error) {
	var all []wire.Entry
	for {
		r, err := s.do(ctx, wire.Request{Op: wire.OpList, After: after, Limit: wire.MaxList})
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
func (s *Session) Get(ctx context.Context, id string) ([]byte, error) {
	r, err := s.do(ctx, wire.Request{Op: wire.OpGet, ID: id})
	if err != nil {
		return nil, err
	}
	if !wire.GoodSize(r.Size) {
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

// Delete removes one blob. The status is wire.StatusDeleted or
// wire.StatusAbsent.
func (s *Session) Delete(ctx context.Context, id string) (string, error) {
	r, err := s.do(ctx, wire.Request{Op: wire.OpDelete, ID: id})
	return r.Status, err
}

// Usage returns the bytes in use, the quota and the number of blobs.
func (s *Session) Usage(ctx context.Context) (used, quota int64, blobs int, err error) {
	r, err := s.do(ctx, wire.Request{Op: wire.OpUsage})
	return r.Used, r.Quota, r.Blobs, err
}
