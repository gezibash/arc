package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/pion/webrtc/v4"
)

// fakeApp is the far end of a pipe. It answers like a storage app, with
// two changes that a client must survive: it answers the final reply of
// each put in reverse order of the requests, after a group of puts, and
// it counts the puts in flight.
type fakeApp struct {
	// older answers hello with invalid_request, as an app before
	// FeatureBatch.
	older bool
	// badID is the ID that the app names in each list reply, and in each
	// reply to a get, delete or audit with ids.
	badID string

	mu       sync.Mutex
	inFlight int
	most     int
	failure  string
}

func (f *fakeApp) fail(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure == "" {
		f.failure = fmt.Sprintf(format, args...)
	}
}

// heldID says which IDs the fake app holds already: it answers held, and
// no bytes go.
func heldID(id string) bool { return id[0] < '4' }

// serve answers the requests of one data channel.
func (f *fakeApp) serve(dc *webrtc.DataChannel, messages <-chan webrtc.DataChannelMessage) {
	reply := func(r Reply) {
		raw, _ := json.Marshal(r)
		_ = dc.SendText(string(raw))
	}
	type sending struct {
		n    uint64
		id   string
		data []byte
		size int64
	}
	var sends []sending
	var done []Reply
	finish := func() {
		// The final replies go in reverse order.
		for _, r := range slices.Backward(done) {
			reply(r)
		}
		f.mu.Lock()
		f.inFlight -= len(done)
		f.mu.Unlock()
		done = nil
	}
	for m := range messages {
		if !m.IsString {
			if len(sends) == 0 {
				f.fail("bytes for no put")
				return
			}
			p := &sends[0]
			p.data = append(p.data, m.Data...)
			if int64(len(p.data)) < p.size {
				continue
			}
			sum := sha256.Sum256(p.data)
			if hex.EncodeToString(sum[:]) != p.id {
				f.fail("the bytes of put %d do not have its ID", p.n)
			}
			done = append(done, Reply{N: p.n, Status: StatusStored})
			sends = sends[1:]
			if len(sends) == 0 || len(done) >= 3 {
				finish()
			}
			continue
		}
		var req Request
		if json.Unmarshal(m.Data, &req) != nil {
			f.fail("a message that is not a request")
			return
		}
		switch {
		case req.Op == OpHello && f.older:
			reply(Reply{Error: ErrInvalidRequest, Message: "a blob ID is 64 lower-case hex digits"})
		case req.Op == OpHello:
			reply(Reply{N: req.N, Version: 2, Features: []string{FeatureBatch, FeatureAudit}, Window: Window})
		case req.Op == OpPut:
			if f.older && req.N != 0 {
				f.fail("a numbered request to an older app")
			}
			f.mu.Lock()
			f.inFlight++
			f.most = max(f.most, f.inFlight)
			f.mu.Unlock()
			if heldID(req.ID) {
				done = append(done, Reply{N: req.N, Status: StatusHeld})
				if len(sends) == 0 {
					finish()
				}
				continue
			}
			sends = append(sends, sending{n: req.N, id: req.ID, size: req.Size})
			reply(Reply{N: req.N, Status: StatusSend})
		case req.Op == OpList && req.After >= 2:
			reply(Reply{N: req.N, Next: req.After})
		case req.Op == OpList:
			reply(Reply{N: req.N, Entries: []Entry{{Seq: 1, ID: strings.Repeat("a", 64), Size: 1}, {Seq: 2, ID: f.badID, Deleted: true}}, Next: 2})
		case req.Op == OpDelete:
			// One item for each ID of the request, the last named badID.
			items := make([]Item, len(req.IDs))
			for i, id := range req.IDs {
				items[i] = Item{ID: id, Status: StatusDeleted}
			}
			items[len(items)-1].ID = f.badID
			reply(Reply{N: req.N, Items: items})
		case req.Op == OpGet || req.Op == OpAudit:
			// The app holds no blob; the last item is named badID.
			items := make([]Item, len(req.IDs))
			for i, id := range req.IDs {
				items[i] = Item{ID: id, Error: ErrNotFound}
			}
			items[len(items)-1].ID = f.badID
			reply(Reply{N: req.N, Items: items})
		default:
			f.fail("unknown operation %q", req.Op)
		}
	}
}

// pipe opens a Conn to a fake app on the loopback address.
func pipe(t *testing.T, app *fakeApp) *Conn {
	t.Helper()
	far, err := direct.NewConnection(direct.Options{Loopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = far.Close() })
	far.OnDataChannel(func(dc *webrtc.DataChannel) {
		messages := make(chan webrtc.DataChannelMessage, 1024)
		dc.OnMessage(func(m webrtc.DataChannelMessage) { messages <- m })
		dc.OnOpen(func() { go app.serve(dc, messages) })
	})
	near, err := direct.NewConnection(direct.Options{Loopback: true})
	if err != nil {
		t.Fatal(err)
	}
	dc, err := near.CreateDataChannel("storage", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, opened := newConn(near, dc)
	t.Cleanup(conn.Close)
	offer, err := near.CreateOffer(nil)
	if err == nil {
		offer, err = direct.Describe(near, offer)
	}
	if err == nil {
		err = far.SetRemoteDescription(offer)
	}
	var answer webrtc.SessionDescription
	if err == nil {
		answer, err = far.CreateAnswer(nil)
	}
	if err == nil {
		answer, err = direct.Describe(far, answer)
	}
	if err == nil {
		err = near.SetRemoteDescription(answer)
	}
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the data channel did not open")
	}
	return conn
}

// blobs makes count blobs of the smallest size. The ID of each is the
// SHA-256 of its bytes.
func blobs(count int) []Blob {
	out := make([]Blob, count)
	for i := range out {
		data := make([]byte, MinBlob)
		copy(data, fmt.Sprintf("blob %d", i))
		sum := sha256.Sum256(data)
		out[i] = Blob{ID: hex.EncodeToString(sum[:]), Data: data}
	}
	return out
}

// TestPutManyWindow checks the requests in flight of PutMany. The client
// keeps at most Window puts in flight. Each result goes to its own blob,
// although the app answers out of order. The bytes of each put reach the
// app in the order of its send replies, so each run has the ID of its put.
func TestPutManyWindow(t *testing.T) {
	app := &fakeApp{}
	conn := pipe(t, app)
	in := blobs(200)
	results, err := conn.PutMany(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.failure != "" {
		t.Fatal(app.failure)
	}
	for i, r := range results {
		want := StatusStored
		if heldID(in[i].ID) {
			want = StatusHeld
		}
		if r.Err != nil || r.Status != want {
			t.Fatalf("put %d (%.8s) gave %q, %v; want %q", i, in[i].ID, r.Status, r.Err, want)
		}
	}
	if app.most > Window {
		t.Fatalf("the client had %d puts in flight; the window is %d", app.most, Window)
	}
	if app.most < Window/2 {
		t.Fatalf("the client had at most %d puts in flight; want about %d", app.most, Window)
	}
}

// TestPutManyOlderApp checks that a client sends one put at a time, with
// no number, to an app that answers hello with invalid_request.
func TestPutManyOlderApp(t *testing.T) {
	app := &fakeApp{older: true}
	conn := pipe(t, app)
	in := blobs(20)
	results, err := conn.PutMany(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	app.mu.Lock()
	defer app.mu.Unlock()
	if app.failure != "" {
		t.Fatal(app.failure)
	}
	if app.most != 1 {
		t.Fatalf("the client had %d puts in flight on an older app; want 1", app.most)
	}
	if !slices.ContainsFunc(results, func(r PutResult) bool { return r.Status == StatusStored }) {
		t.Fatalf("no put stored: %v", results)
	}
}

// TestAttemptEndsWhenICEFails gives the client the answer of a far end
// that closes at once, so no check gets an answer. ICE fails after about
// 8 s (docs/direct/SPEC.md, section 3.2), and the attempt ends then, not
// after 12 s.
func TestAttemptEndsWhenICEFails(t *testing.T) {
	loopback := direct.Options{Loopback: true}
	c := Client{Call: func(_ context.Context, session string) ([]byte, error) {
		var s Session
		if err := json.Unmarshal([]byte(session), &s); err != nil {
			return nil, err
		}
		far, err := direct.NewConnection(loopback)
		if err != nil {
			return nil, err
		}
		defer far.Close()
		if err := far.SetRemoteDescription(s.SDP); err != nil {
			return nil, err
		}
		answer, err := far.CreateAnswer(nil)
		if err == nil {
			answer, err = direct.Describe(far, answer)
		}
		if err != nil {
			return nil, err
		}
		return json.Marshal(Answer{Version: 1, SDP: answer})
	}}
	start := time.Now()
	_, err := c.attempt(t.Context(), loopback, 0)
	took := time.Since(start)
	if !errors.Is(err, direct.ErrNoPath) {
		t.Fatalf("error %v, want %v", err, direct.ErrNoPath)
	}
	if took >= direct.OpenWait-time.Second {
		t.Errorf("the attempt ended after %s, want about 8 s", took.Round(100*time.Millisecond))
	}
}

// A storage citizen that names an ID that is not a blob ID, in a list or in
// a delete, gets its reply refused; the error names the ID escaped, on one
// line.
func TestAReplyWithABadIDIsRefused(t *testing.T) {
	bad := "P\x1b]0;pwned\x07\x1b[2J\u202egnp\narc-storage: forged"
	shown := `P\x1b]0;pwned\x07\x1b[2J\u202egnp\narc-storage: forged`
	conn := pipe(t, &fakeApp{badID: bad})
	entries, _, err := conn.List(context.Background(), 0)
	if err == nil || len(entries) != 0 || !strings.Contains(err.Error(), shown) || strings.ContainsAny(err.Error(), "\x1b\n") {
		t.Errorf("List gave %v, %v; want no entries and an error that names %q", entries, err, shown)
	}
	items, err := conn.DeleteMany(context.Background(), []string{strings.Repeat("b", 64)})
	if err == nil || len(items) != 0 || !strings.Contains(err.Error(), shown) || strings.ContainsAny(err.Error(), "\x1b\n") {
		t.Errorf("DeleteMany gave %v, %v; want no items and an error that names %q", items, err, shown)
	}
	called := 0
	err = conn.GetMany(context.Background(), []string{strings.Repeat("b", 64)}, func(string, []byte, error) error {
		called++
		return nil
	})
	if err == nil || called != 0 || !strings.Contains(err.Error(), shown) || strings.ContainsAny(err.Error(), "\x1b\n") {
		t.Errorf("GetMany gave %v after %d items; want no items and an error that names %q", err, called, shown)
	}
	items, err = conn.Audit(context.Background(), []string{strings.Repeat("b", 64)}, NewNonce())
	if err == nil || len(items) != 0 || !strings.Contains(err.Error(), shown) || strings.ContainsAny(err.Error(), "\x1b\n") {
		t.Errorf("Audit gave %v, %v; want no items and an error that names %q", items, err, shown)
	}
	good := pipe(t, &fakeApp{badID: strings.Repeat("c", 64)})
	if entries, _, err := good.List(context.Background(), 0); err != nil || len(entries) != 2 {
		t.Errorf("a list of good IDs gave %v, %v", entries, err)
	}
}
