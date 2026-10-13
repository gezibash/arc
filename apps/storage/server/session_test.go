package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	if hello.N != 7 || hello.Version != 2 || !slices.Equal(hello.Features, []string{wire.FeatureBatch, wire.FeaturePieces, wire.FeatureAudit, wire.FeatureDrain}) || hello.Window != wire.Window {
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

// TestPieceTags checks the feature pieces: the app keeps the tag of a
// put, gives it back in each listing, also after a restart, and takes the
// size of a piece only with a tag. A blob of format 1 still has no tag.
func TestPieceTags(t *testing.T) {
	dir := t.TempDir()
	s := testServer(t, dir)
	conn := testConn(t, s)
	ctx := context.Background()
	if pieces, err := conn.Pieces(ctx); err != nil || !pieces {
		t.Fatalf("the app gives pieces = %v, %v", pieces, err)
	}
	tag := bytes.Repeat([]byte{7}, wire.TagSize)
	piece := wire.Blob{ID: strings.Repeat("1", 64), Data: make([]byte, wire.PieceSize(wire.MinBlob, 2)), Tag: tag}
	whole := wire.Blob{ID: strings.Repeat("2", 64), Data: make([]byte, wire.MinBlob)}
	untagged := wire.Blob{ID: strings.Repeat("3", 64), Data: make([]byte, wire.PieceSize(wire.MinBlob, 2))}
	short := wire.Blob{ID: strings.Repeat("4", 64), Data: make([]byte, wire.PieceSize(wire.MinBlob, 2)), Tag: tag[:40]}
	results, err := conn.PutMany(ctx, []wire.Blob{piece, whole, untagged, short})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Status != wire.StatusStored || results[1].Status != wire.StatusStored {
		t.Fatalf("the piece and the blob gave %+v and %+v; want stored", results[0], results[1])
	}
	if !wire.IsRefused(results[2].Err, wire.ErrBadSize) {
		t.Fatalf("a piece with no tag gave %+v; want bad_size", results[2])
	}
	if !wire.IsRefused(results[3].Err, wire.ErrInvalidRequest) {
		t.Fatalf("a tag of 40 bytes gave %+v; want invalid_request", results[3])
	}
	check := func(conn *wire.Conn, when string) {
		t.Helper()
		entries, _, err := conn.List(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		tags := map[string]string{}
		for _, e := range entries {
			tags[e.ID] = e.Tag
		}
		if len(entries) != 2 || tags[piece.ID] != base64.StdEncoding.EncodeToString(tag) || tags[whole.ID] != "" {
			t.Fatalf("%s, the app lists %+v", when, entries)
		}
		var got []byte
		if err := conn.GetMany(ctx, []string{piece.ID}, func(_ string, data []byte, err error) error { got = data; return err }); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(piece.Data) {
			t.Fatalf("%s, a get of the piece gave %d bytes, want %d", when, len(got), len(piece.Data))
		}
	}
	check(conn, "after the puts")
	conn.Close()
	if err := s.store.close(); err != nil {
		t.Fatal(err)
	}
	check(testConn(t, testServer(t, dir)), "after a restart")
}

// TestAuditReplay audits one blob twice through the real app and client,
// with two nonces. Each answer is the proof of the bytes for its own
// nonce. The answer to the first nonce does not pass for the second, so a
// storage citizen that keeps old answers in place of the bytes fails.
func TestAuditReplay(t *testing.T) {
	s := testServer(t, t.TempDir())
	conn := testConn(t, s)
	ctx := context.Background()
	b := testBlobs(1, wire.MinBlob)[0]
	if _, err := conn.Put(ctx, b.ID, b.Data); err != nil {
		t.Fatal(err)
	}
	first, second := wire.NewNonce(), wire.NewNonce()
	old, err := conn.Audit(ctx, []string{b.ID}, first)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := conn.Audit(ctx, []string{b.ID}, second)
	if err != nil {
		t.Fatal(err)
	}
	if old[0].Proof != wire.AuditProof(first, b.ID, b.Data) || fresh[0].Proof != wire.AuditProof(second, b.ID, b.Data) {
		t.Fatalf("the answers %+v and %+v are not the proofs of the bytes", old[0], fresh[0])
	}
	if old[0].Proof == wire.AuditProof(second, b.ID, b.Data) {
		t.Fatal("the answer to the first nonce passes an audit with the second nonce")
	}
}

// TestGetGoneFile gets a blob whose entry stays and whose file is gone.
// The app answers not_found for it, sends the bytes of the other blob,
// and keeps the pipe open. Repair marks such a piece lost only if the get
// answers.
func TestGetGoneFile(t *testing.T) {
	s := testServer(t, t.TempDir())
	conn := testConn(t, s)
	ctx := context.Background()
	blobs := testBlobs(2, wire.MinBlob)
	for _, b := range blobs {
		if _, err := conn.Put(ctx, b.ID, b.Data); err != nil {
			t.Fatal(err)
		}
	}
	gone, kept := blobs[0], blobs[1]
	if err := os.Remove(s.store.blobPath(testAccount, gone.ID)); err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	refused := map[string]error{}
	err := conn.GetMany(ctx, []string{gone.ID, kept.ID}, func(id string, blob []byte, err error) error {
		got[id], refused[id] = blob, err
		return nil
	})
	if err != nil {
		t.Fatalf("get of a gone file and a kept file: %v", err)
	}
	if !wire.IsRefused(refused[gone.ID], wire.ErrNotFound) {
		t.Fatalf("the gone file got %v, not not_found", refused[gone.ID])
	}
	if refused[kept.ID] != nil || !bytes.Equal(got[kept.ID], kept.Data) {
		t.Fatalf("the kept file got %d bytes and %v", len(got[kept.ID]), refused[kept.ID])
	}
	if _, err := conn.Get(ctx, gone.ID); !wire.IsRefused(err, wire.ErrNotFound) {
		t.Fatalf("a get of one gone file got %v, not not_found", err)
	}
	if _, err := conn.Usage(ctx); err != nil {
		t.Fatalf("the pipe ended after the gone file: %v", err)
	}
}

// TestAPutGoesToTheDiskAsItComes checks that the app writes the bytes of a
// put to a temporary file as they come, so a put of 64 MiB never waits
// whole in memory, and that a pipe that ends in the middle of a put leaves
// no file.
func TestAPutGoesToTheDiskAsItComes(t *testing.T) {
	s := testServer(t, t.TempDir())
	dc, messages, _ := rawPipe(t, s)
	blob := testBlobs(1, 1<<20)[0]
	sendJSON(t, dc, wire.Request{Op: wire.OpPut, ID: blob.ID, Size: int64(len(blob.Data))})
	if r := readReply(t, messages); r.Status != wire.StatusSend {
		t.Fatalf("the put got %+v, not send", r)
	}
	half := len(blob.Data) / 2
	for at := 0; at < half; at += 16 << 10 {
		if err := dc.Send(blob.Data[at : at+16<<10]); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Dir(s.store.blobPath(testAccount, blob.ID))
	temporary := func() (names []string, size int64) {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "."+blob.ID) {
				// The app can remove the file between ReadDir and Info.
				info, err := e.Info()
				if err != nil {
					continue
				}
				names, size = append(names, e.Name()), info.Size()
			}
		}
		return names, size
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, size := temporary(); size < int64(half); _, size = temporary() {
		if time.Now().After(deadline) {
			t.Fatalf("after half the bytes, the temporary file holds %d bytes, want %d", size, half)
		}
		time.Sleep(10 * time.Millisecond)
	}

	_ = dc.Close()
	deadline = time.Now().Add(10 * time.Second)
	for names, _ := temporary(); len(names) > 0; names, _ = temporary() {
		if time.Now().After(deadline) {
			t.Fatalf("the put that did not end left %v", names)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.Stat(s.store.blobPath(testAccount, blob.ID)); !os.IsNotExist(err) {
		t.Fatalf("the put that did not end left its blob: %v", err)
	}
}

// TestCloseCall checks the close of an account by its account key. The app
// refuses the close of an account that holds a blob. The close of an empty
// account ends the open pipe of the account, and removes its directory. A
// device key, and a key with no account, get not_owner.
func TestCloseCall(t *testing.T) {
	dir := t.TempDir()
	s := testServer(t, dir)
	conn := testConn(t, s)
	ctx := context.Background()
	closeAs := func(from string) (wire.CloseReply, error) {
		t.Helper()
		reply, err := s.HandleRequest(ctx, provider.Request{From: from, Message: `{"v":1,"close":{}}`})
		var out wire.CloseReply
		if err == nil && json.Unmarshal([]byte(reply), &out) != nil {
			t.Fatalf("the close gave %q, not a close reply", reply)
		}
		return out, err
	}
	blob := testBlobs(1, wire.MinBlob)[0]
	if status, err := conn.Put(ctx, blob.ID, blob.Data); err != nil || status != wire.StatusStored {
		t.Fatalf("put gave %q, %v", status, err)
	}

	reply, err := closeAs(testAccount)
	if err != nil || reply.Closed || reply.Error != wire.ErrRefused || !strings.Contains(reply.Message, "the account holds 1 blobs") {
		t.Fatalf("the close of an account with a blob gave %+v, %v; want refused", reply, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts", testAccount, "blobs", blob.ID)); err != nil {
		t.Fatalf("the refused close removed the blob: %v", err)
	}
	if _, err := closeAs(strings.Repeat("d", 64)); err == nil || err.Error() != string(errNotOwner) {
		t.Fatalf("the close of a key with no account gave %v, want not_owner", err)
	}

	if _, err := conn.Delete(ctx, blob.ID); err != nil {
		t.Fatal(err)
	}
	reply, err = closeAs(testAccount)
	if err != nil || !reply.Closed {
		t.Fatalf("the close of an empty account gave %+v, %v; want closed", reply, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "accounts", testAccount)); !os.IsNotExist(err) {
		t.Fatalf("the directory of the closed account: %v; want it removed", err)
	}
	if _, err := conn.Usage(ctx); err == nil {
		t.Fatal("the pipe of the account still answers after the close")
	}
}
