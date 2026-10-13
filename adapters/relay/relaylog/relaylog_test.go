package relaylog_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"log/slog"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"

	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/relaylog"
)

var ctx = context.Background()

// buffer is a log that the connection goroutines of the relay write to.
type buffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *buffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// lines are the log lines with the message msg.
func (b *buffer) lines(msg string) []map[string]any {
	var found []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && m["msg"] == msg {
			found = append(found, m)
		}
	}
	return found
}

// wait waits until the log has a line with the message msg.
func (b *buffer) wait(t *testing.T, msg string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if found := b.lines(msg); len(found) > 0 {
			return found[0]
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no line %q in the log:\n%s", msg, b.String())
	return nil
}

// start runs a relay as arc relay serve runs it, with the policy, and
// returns its address and its log.
func start(t *testing.T, p limits.Policy) (string, *relaylog.Log, *buffer) {
	t.Helper()
	db := &boltdb.BoltBackend{Path: filepath.Join(t.TempDir(), "relay.db")}
	if err := db.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	out := &buffer{}
	connections := relaylog.New(slog.New(slog.NewJSONHandler(out, nil)))
	p.Refused = connections.Refused
	rl := khatru.NewRelay()
	rl.UseEventstore(db, 500)
	limits.Apply(rl, db.DB, p)
	connections.Watch(rl)
	server := httptest.NewUnstartedServer(rl)
	server.Listener = connections.Listen(server.Listener)
	server.Config.ConnContext = connections.ConnContext
	server.Start()
	t.Cleanup(server.Close)
	return server.Listener.Addr().String(), connections, out
}

func note(content string) nostr.Event {
	e := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: content}
	e.Sign(nostr.Generate())
	return e
}

// upgrade opens a WebSocket by hand, so that the test can drop the TCP
// connection without a close frame.
func upgrade(t *testing.T, address string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(c, "GET / HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n\r\n", address)
	response, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("the upgrade answered %s", response.Status)
	}
	return c
}

// A request that asks for a WebSocket without the Connection header fails
// the upgrade. The log has a WARN line with the reason.
func TestAFailedUpgradeHasALine(t *testing.T) {
	address, _, out := start(t, limits.Policy{})
	request, _ := http.NewRequest(http.MethodGet, "http://"+address, nil)
	request.Header.Set("Upgrade", "websocket")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()

	line := out.wait(t, "websocket upgrade failed")
	if line["level"] != "WARN" {
		t.Errorf("level %v, want WARN", line["level"])
	}
	if reason, _ := line["reason"].(string); !strings.Contains(reason, "'upgrade' token not found in 'Connection' header") {
		t.Errorf("reason %q does not say why the upgrade failed", reason)
	}
}

// A second event from one address over a burst of one is refused by the
// rate limiter. The log has one WARN line with the reason, and no IP address.
func TestARefusalHasALineWithoutTheAddress(t *testing.T) {
	address, _, out := start(t, limits.Policy{Rate: 60, Burst: 1})
	relay, err := nostr.RelayConnect(ctx, "ws://"+address, nostr.RelayOptions{NoticeHandler: func(*nostr.Relay, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	defer relay.Close()
	if err := relay.Publish(ctx, note("one")); err != nil {
		t.Fatal(err)
	}
	if err := relay.Publish(ctx, note("two")); err == nil {
		t.Fatal("the rate limiter took the second event")
	}

	line := out.wait(t, "event refused")
	if line["level"] != "WARN" || line["reason"] != "rate" {
		t.Errorf("line %v, want level WARN and reason rate", line)
	}
	if n := len(out.lines("event refused")); n != 1 {
		t.Errorf("%d refusal lines, want 1", n)
	}
	if strings.Contains(out.String(), "127.0.0.1") {
		t.Errorf("the log holds the IP address:\n%s", out.String())
	}
}

// A client that resets the TCP connection closes it with an error. The log
// has a WARN line with the duration and the reason. A client that closes
// normally has no line.
func TestAResetConnectionHasALineAndANormalCloseHasNone(t *testing.T) {
	address, _, out := start(t, limits.Policy{})
	relay, err := nostr.RelayConnect(ctx, "ws://"+address, nostr.RelayOptions{})
	if err != nil {
		t.Fatal(err)
	}
	_ = relay.Close()
	time.Sleep(200 * time.Millisecond)
	if found := out.lines("connection closed with an error"); len(found) > 0 {
		t.Fatalf("a normal close has a line: %v", found)
	}

	c := upgrade(t, address)
	time.Sleep(50 * time.Millisecond)
	_ = c.(*net.TCPConn).SetLinger(0) // the close sends a reset
	_ = c.Close()

	line := out.wait(t, "connection closed with an error")
	if line["level"] != "WARN" {
		t.Errorf("level %v, want WARN", line["level"])
	}
	if reason, _ := line["reason"].(string); !strings.Contains(reason, "reset") {
		t.Errorf("reason %q, want a reset", reason)
	}
	if d, _ := line["duration"].(float64); time.Duration(d) < 50*time.Millisecond {
		t.Errorf("duration %v, want at least 50ms", time.Duration(d))
	}
}

// The summary counts the open, opened and closed connections and the
// refusals of one interval. An interval in which nothing happened has no
// summary.
func TestTheSummaryCountsTheInterval(t *testing.T) {
	address, connections, out := start(t, limits.Policy{Rate: 60, Burst: 1})
	ctx, stop := context.WithCancel(ctx)
	defer stop()
	go connections.Run(ctx, 300*time.Millisecond)

	held := upgrade(t, address)
	defer held.Close()
	relay, err := nostr.RelayConnect(ctx, "ws://"+address, nostr.RelayOptions{NoticeHandler: func(*nostr.Relay, string) {}})
	if err != nil {
		t.Fatal(err)
	}
	_ = relay.Publish(ctx, note("one"))
	_ = relay.Publish(ctx, note("two"))
	_ = relay.Close()

	// A tick can fall inside the activity, so the counts can spread over
	// more than one summary.
	want := map[string]float64{"opened": 2, "closed": 1, "closed_with_error": 0, "refused_rate": 1}
	var total map[string]float64
	var last map[string]any
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		total = map[string]float64{}
		for _, line := range out.lines("relay summary") {
			last = line
			for key := range want {
				n, _ := line[key].(float64)
				total[key] += n
			}
		}
		if total["closed"] == want["closed"] && total["refused_rate"] == want["refused_rate"] {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	for key, n := range want {
		if total[key] != n {
			t.Errorf("%s = %v over the summaries, want %v:\n%s", key, total[key], n, out.String())
		}
	}
	if last["open"] != 1.0 || last["level"] != "INFO" {
		t.Errorf("last summary %v, want level INFO and open 1", last)
	}

	n := len(out.lines("relay summary"))
	time.Sleep(time.Second)
	if after := len(out.lines("relay summary")); after != n {
		t.Errorf("%d summaries after a quiet second, want none", after-n)
	}
}
