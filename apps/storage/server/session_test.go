package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/pion/webrtc/v4"
)

const testAccount = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// testServer serves the storage app on the loopback address for one owner.
func testServer(t *testing.T, dir string) *server {
	t.Helper()
	s := &server{
		store: &store{dir: dir, owners: []string{testAccount}, quota: DefaultQuota, now: time.Now},
		rtc:   direct.Options{Loopback: true},
		slots: make(chan struct{}, MaxSessions),
		log:   io.Discard,
	}
	t.Cleanup(func() { _ = s.store.close() })
	return s
}

// call sends an ARC call that opens a session, as the manifest command
// "open" does.
func (s *server) call(ctx context.Context, session string) ([]byte, error) {
	open := wire.Open{Version: 1}
	if err := json.Unmarshal([]byte(session), &open.Session); err != nil {
		return nil, err
	}
	body, err := json.Marshal(open)
	if err != nil {
		return nil, err
	}
	reply, err := s.HandleRequest(ctx, provider.Request{From: testAccount, Message: string(body)})
	return []byte(reply), err
}

func testConn(t *testing.T, s *server) *wire.Conn {
	t.Helper()
	conn, err := wire.Client{Call: s.call, Options: direct.Options{Loopback: true}, NoRelay: true}.Open(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(conn.Close)
	return conn
}

// testBlobs makes count blobs. The ID of each is the SHA-256 of its
// bytes, so a test sees if bytes land on another ID.
func testBlobs(count int, size int) []wire.Blob {
	out := make([]wire.Blob, count)
	for i := range out {
		data := make([]byte, size)
		copy(data, fmt.Sprintf("blob %d of %d", i, count))
		sum := sha256.Sum256(data)
		out[i] = wire.Blob{ID: hex.EncodeToString(sum[:]), Data: data}
	}
	return out
}

// TestRequestsInFlight puts more blobs than the window through the real
// app and client, and then gets and deletes them by lists of IDs. Each
// blob keeps its own bytes, and the sequence numbers follow the order of
// the puts.
func TestRequestsInFlight(t *testing.T) {
	s := testServer(t, t.TempDir())
	conn := testConn(t, s)
	ctx := context.Background()
	in := testBlobs(3*wire.Window+5, wire.MinBlob)
	// A blob of 16 KiB runs over several messages of the pipe.
	in = append(in, testBlobs(4, 4*wire.MinBlob)...)
	// The same ID twice in one window: the second gets held while the
	// first waits for its bytes or its entry.
	in = append(in, in[len(in)-1])

	results, err := conn.PutMany(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range results[:len(in)-1] {
		if r.Err != nil || r.Status != wire.StatusStored {
			t.Fatalf("put %d gave %q, %v; want stored", i, r.Status, r.Err)
		}
	}
	if last := results[len(in)-1]; last.Status != wire.StatusHeld {
		t.Fatalf("the second put of one ID gave %q, %v; want held", last.Status, last.Err)
	}

	entries, _, err := conn.List(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(in)-1 {
		t.Fatalf("listed %d entries, want %d", len(entries), len(in)-1)
	}
	for i, e := range entries {
		if e.ID != in[i].ID || e.Seq != uint64(i+1) || e.Size != int64(len(in[i].Data)) {
			t.Fatalf("entry %d is %+v; want seq %d, ID %.8s, size %d", i, e, i+1, in[i].ID, len(in[i].Data))
		}
	}

	ids := []string{strings.Repeat("0", 64)}
	want := map[string][]byte{}
	for _, b := range in {
		ids = append(ids, b.ID)
		want[b.ID] = b.Data
	}
	got := 0
	err = conn.GetMany(ctx, ids, func(id string, blob []byte, err error) error {
		if id == ids[0] {
			if !wire.IsRefused(err, wire.ErrNotFound) {
				t.Errorf("an unknown ID gave %v, want not_found", err)
			}
			return nil
		}
		if err != nil || !bytes.Equal(blob, want[id]) {
			t.Errorf("get %.8s gave %d bytes, %v; want its own %d bytes", id, len(blob), err, len(want[id]))
		}
		got++
		return nil
	})
	if err != nil || got != len(ids)-1 {
		t.Fatalf("GetMany gave %d blobs, %v; want %d", got, err, len(ids)-1)
	}

	items, err := conn.DeleteMany(ctx, []string{in[0].ID, ids[0], in[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	statuses := []string{items[0].Status, items[1].Status, items[2].Status}
	if strings.Join(statuses, " ") != "deleted absent deleted" {
		t.Fatalf("DeleteMany gave %v, want deleted absent deleted", statuses)
	}
	usage, err := conn.Usage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var used int64
	for _, b := range in[2 : len(in)-1] {
		used += int64(len(b.Data))
	}
	if usage.Blobs != len(in)-3 || usage.Used != used {
		t.Fatalf("usage gave %d blobs, %d bytes; want %d, %d", usage.Blobs, usage.Used, len(in)-3, used)
	}
}

// rawPipe opens a session to the app with a data channel that the test
// drives by hand.
func rawPipe(t *testing.T, s *server) (*webrtc.DataChannel, chan webrtc.DataChannelMessage, chan struct{}) {
	t.Helper()
	pc, err := direct.NewConnection(direct.Options{Loopback: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	dc, err := pc.CreateDataChannel("storage", nil)
	if err != nil {
		t.Fatal(err)
	}
	messages := make(chan webrtc.DataChannelMessage, 1024)
	opened, closed := make(chan struct{}), make(chan struct{})
	dc.OnOpen(func() { close(opened) })
	dc.OnClose(func() { close(closed) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) { messages <- m })
	offer, err := pc.CreateOffer(nil)
	if err == nil {
		offer, err = direct.Describe(pc, offer)
	}
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(wire.Session{SDP: offer})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.call(context.Background(), string(body))
	if err != nil {
		t.Fatal(err)
	}
	var answer wire.Answer
	if err := json.Unmarshal(raw, &answer); err != nil {
		t.Fatal(err)
	}
	if err := pc.SetRemoteDescription(answer.SDP); err != nil {
		t.Fatal(err)
	}
	select {
	case <-opened:
	case <-time.After(10 * time.Second):
		t.Fatal("the data channel did not open")
	}
	return dc, messages, closed
}

func sendJSON(t *testing.T, dc *webrtc.DataChannel, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := dc.SendText(string(raw)); err != nil {
		t.Fatal(err)
	}
}

func readReply(t *testing.T, messages chan webrtc.DataChannelMessage) wire.Reply {
	t.Helper()
	select {
	case m := <-messages:
		var r wire.Reply
		if !m.IsString || json.Unmarshal(m.Data, &r) != nil {
			t.Fatalf("the app sent %q, which is not a reply", m.Data)
		}
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the app did not answer")
	}
	return wire.Reply{}
}

// TestHelloAndWindow checks the reply to hello, and that the app ends a
// pipe whose client keeps more puts in flight than the window.
func TestHelloAndWindow(t *testing.T) {
	s := testServer(t, t.TempDir())
	dc, messages, closed := rawPipe(t, s)
	sendJSON(t, dc, wire.Request{Op: wire.OpHello, N: 7})
	hello := readReply(t, messages)
	if hello.N != 7 || hello.Version != 2 || len(hello.Features) != 1 || hello.Features[0] != wire.FeatureBatch || hello.Window != wire.Window {
		t.Fatalf("hello gave %+v", hello)
	}

	in := testBlobs(wire.Window+1, wire.MinBlob)
	for i, b := range in {
		sendJSON(t, dc, wire.Request{Op: wire.OpPut, N: uint64(i + 1), ID: b.ID, Size: wire.MinBlob})
	}
	// The app may end the pipe before each send reply reaches the
	// client, so the test reads until the end.
	for {
		select {
		case m := <-messages:
			var r wire.Reply
			if json.Unmarshal(m.Data, &r) != nil || r.Status != wire.StatusSend || r.N > wire.Window {
				t.Fatalf("the app answered %q; want send for puts 1 to %d only", m.Data, wire.Window)
			}
		case <-closed:
			return
		case <-time.After(10 * time.Second):
			t.Fatalf("the app kept the pipe open with %d puts in flight", wire.Window+1)
		}
	}
}
