package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
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

// Put gives one blob. The size of blob must be a power of two, at least
// MinBlob: see Pad. The status is StatusStored,
// StatusHeld or StatusTombstoned.
func (s *Conn) Put(ctx context.Context, id string, blob []byte) (string, error) {
	return s.put(ctx, Blob{ID: id, Data: blob})
}

// tagOf is the tag of a put request: base64 of the tag, or nothing.
func tagOf(b Blob) string {
	if b.Tag == nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(b.Tag)
}

func (s *Conn) put(ctx context.Context, b Blob) (string, error) {
	blob := b.Data
	r, err := s.do(ctx, Request{Op: OpPut, ID: b.ID, Size: int64(len(blob)), Tag: tagOf(b)})
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
	return s.blob(ctx, r.Size)
}

// blob reads the bytes of one blob or piece of a get.
func (s *Conn) blob(ctx context.Context, size int64) ([]byte, error) {
	if !GoodSize(size) && !GoodPieceSize(size) {
		return nil, fmt.Errorf("the storage citizen announced a blob of %d bytes", size)
	}
	data := make([]byte, 0, size)
	for int64(len(data)) < size {
		m, err := s.next(ctx)
		if err != nil {
			return nil, err
		}
		if m.IsString || int64(len(data)+len(m.Data)) > size {
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

// Window asks the app for its features once, with hello, and returns the
// count of requests that the device keeps in flight: Window or less for an
// app with FeatureBatch, and 1 for an older app.
func (s *Conn) Window(ctx context.Context) (int, error) {
	if s.window > 0 {
		return s.window, nil
	}
	r, err := s.do(ctx, Request{Op: OpHello})
	s.features = r.Features
	switch {
	case IsRefused(err, ErrInvalidRequest):
		s.window = 1
	case err != nil:
		return 0, err
	case slices.Contains(r.Features, FeatureBatch) && r.Window > 1:
		s.window = min(r.Window, Window)
	default:
		s.window = 1
	}
	return s.window, nil
}

// Pieces says if the app keeps the tags of pieces. It asks hello once. An
// older app ignores a tag, so a device puts only blobs of format 1 on it.
func (s *Conn) Pieces(ctx context.Context) (bool, error) {
	return s.Has(ctx, FeaturePieces)
}

// PutResult is the result of one put of PutMany: a status of Put, or a
// refusal.
type PutResult struct {
	Status string
	Err    error
}

// PutMany gives several blobs, with at most Window puts in flight. Each
// result is for the blob at the same index. A refusal of one put is in
// its result, and the other puts go on. The error says that the pipe
// failed; the results then hold the puts that ended before.
func (s *Conn) PutMany(ctx context.Context, blobs []Blob) ([]PutResult, error) {
	results := make([]PutResult, len(blobs))
	if len(blobs) == 0 {
		return results, nil
	}
	window, err := s.Window(ctx)
	if err != nil {
		return results, err
	}
	if window <= 1 {
		for i, b := range blobs {
			status, err := s.put(ctx, b)
			if err != nil && !IsRefused(err, "") {
				return results, err
			}
			results[i] = PutResult{Status: status, Err: err}
		}
		return results, nil
	}

	// One writer sends the bytes, in the order of the send replies. The
	// app gives each run of bytes to the oldest put that it answered send.
	bytesOf := make(chan []byte, window)
	failed := make(chan error, 1)
	go func() {
		for data := range bytesOf {
			if err := direct.Send(s.dc, bytes.NewReader(data), int64(len(data))); err != nil {
				failed <- err
				for range bytesOf {
				}
				return
			}
		}
	}()
	defer close(bytesOf)

	inFlight := map[uint64]int{}
	next := 0
	for next < len(blobs) || len(inFlight) > 0 {
		for next < len(blobs) && len(inFlight) < window {
			s.n++
			if err := s.send(Request{Op: OpPut, N: s.n, ID: blobs[next].ID, Size: int64(len(blobs[next].Data)), Tag: tagOf(blobs[next])}); err != nil {
				return results, err
			}
			inFlight[s.n] = next
			next++
		}
		select {
		case err := <-failed:
			return results, err
		default:
		}
		r, err := s.reply(ctx)
		if err != nil && !IsRefused(err, "") {
			return results, err
		}
		i, ok := inFlight[r.N]
		if !ok {
			return results, fmt.Errorf("the storage citizen answered the request %d, which is not in flight", r.N)
		}
		switch {
		case err != nil:
			results[i].Err = err
			delete(inFlight, r.N)
		case r.Status == StatusSend:
			bytesOf <- blobs[i].Data
		default:
			results[i].Status = r.Status
			delete(inFlight, r.N)
		}
	}
	return results, nil
}

// GetMany gets several blobs, MaxBatch IDs in one request. It calls f for
// each ID, in order, with its bytes or a refusal, such as not_found. If f
// returns an error, GetMany reads the rest of the bytes of that request,
// and returns the error.
func (s *Conn) GetMany(ctx context.Context, ids []string, f func(id string, blob []byte, err error) error) error {
	if len(ids) == 0 {
		return nil
	}
	window, err := s.Window(ctx)
	if err != nil {
		return err
	}
	if window <= 1 {
		for _, id := range ids {
			blob, err := s.Get(ctx, id)
			if err != nil && !IsRefused(err, "") {
				return err
			}
			if err := f(id, blob, err); err != nil {
				return err
			}
		}
		return nil
	}
	for chunk := range slices.Chunk(ids, MaxBatch) {
		r, err := s.batch(ctx, Request{Op: OpGet, IDs: chunk})
		if err != nil {
			return err
		}
		var stop error
		for i, item := range r.Items {
			var blob []byte
			var refused error
			if item.Error != "" {
				message := item.Error
				if item.Error == ErrNotFound {
					message = "the account holds no blob " + item.ID
				}
				refused = &Refused{Code: item.Error, Message: message}
			} else if blob, err = s.blob(ctx, item.Size); err != nil {
				return err
			}
			if stop == nil {
				stop = f(chunk[i], blob, refused)
			}
		}
		if stop != nil {
			return stop
		}
	}
	return nil
}

// DeleteMany removes several blobs, MaxBatch IDs in one request. Each item
// is for the ID at the same index: StatusDeleted or StatusAbsent, or an
// error code.
func (s *Conn) DeleteMany(ctx context.Context, ids []string) ([]Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	window, err := s.Window(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(ids))
	if window <= 1 {
		for _, id := range ids {
			status, err := s.Delete(ctx, id)
			item := Item{ID: id, Status: status}
			if r, ok := errors.AsType[*Refused](err); ok {
				item.Error = r.Code
			} else if err != nil {
				return items, err
			}
			items = append(items, item)
		}
		return items, nil
	}
	for chunk := range slices.Chunk(ids, MaxBatch) {
		r, err := s.batch(ctx, Request{Op: OpDelete, IDs: chunk})
		if err != nil {
			return items, err
		}
		items = append(items, r.Items...)
	}
	return items, nil
}

// Has says if the app announced a feature in its reply to hello. An older
// app announces none.
func (s *Conn) Has(ctx context.Context, feature string) (bool, error) {
	if _, err := s.Window(ctx); err != nil {
		return false, err
	}
	return slices.Contains(s.features, feature), nil
}

// ErrOlderApp says that the storage app does not know a request. It runs
// an older version.
var ErrOlderApp = errors.New("the storage citizen runs an older storage app; it must update")

// Audit asks the app to prove that it holds blobs, MaxBatch IDs in one
// request. Each item is for the ID at the same index: Size and Proof, or
// an error code. The caller compares each Proof with the AuditProof of the
// bytes that it expects. nonce must be NewNonce, new for this audit.
func (s *Conn) Audit(ctx context.Context, ids []string, nonce []byte) ([]Item, error) {
	if ok, err := s.Has(ctx, FeatureAudit); err != nil || !ok {
		if err == nil {
			err = ErrOlderApp
		}
		return nil, err
	}
	items := make([]Item, 0, len(ids))
	for chunk := range slices.Chunk(ids, MaxBatch) {
		r, err := s.batch(ctx, Request{Op: OpAudit, IDs: chunk, Nonce: hex.EncodeToString(nonce)})
		if err != nil {
			return items, err
		}
		items = append(items, r.Items...)
	}
	return items, nil
}

// Drain sets the account to StateDraining: the app refuses each put, and
// keeps each read, list and delete. off sets it back to StateOpen. The
// result tells what remains.
func (s *Conn) Drain(ctx context.Context, off bool) (Usage, error) {
	if ok, err := s.Has(ctx, FeatureDrain); err != nil || !ok {
		if err == nil {
			err = ErrOlderApp
		}
		return Usage{}, err
	}
	r, err := s.do(ctx, Request{Op: OpDrain, Off: off})
	return Usage{Used: r.Used, Quota: r.Quota, Blobs: r.Blobs, State: r.State, Plan: r.Plan, Expires: r.Expires}, err
}

// batch sends one get, delete or audit with IDs, and checks that the reply has
// one item for each ID, in order.
func (s *Conn) batch(ctx context.Context, req Request) (Reply, error) {
	s.n++
	n := s.n
	req.N = n
	ids := req.IDs
	r, err := s.do(ctx, req)
	if err != nil {
		return r, err
	}
	if r.N != n || len(r.Items) != len(ids) {
		return r, fmt.Errorf("the storage citizen answered %d items for request %d, not %d items for request %d", len(r.Items), r.N, len(ids), n)
	}
	for i, item := range r.Items {
		if item.ID != ids[i] {
			return r, fmt.Errorf("the storage citizen answered %s in place of %s", item.ID, ids[i])
		}
	}
	return r, nil
}
