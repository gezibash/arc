package citizen

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gezibash/arc/adapters/transport/relay"
)

// A file of down relays that is missing, does not parse, or lies in a
// folder that arc cannot write, is no record: nothing fails (#439).
func TestABrokenFileOfDownRelaysIsNoRecord(t *testing.T) {
	dir := t.TempDir()
	d := relaysDown{path: filepath.Join(dir, "relays-down.json")}
	if _, _, ok := d.Down("ws://a"); ok {
		t.Fatal("a missing file holds a record")
	}
	if err := os.WriteFile(d.path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.Down("ws://a"); ok {
		t.Fatal("a file that does not parse holds a record")
	}
	d.MarkDown("ws://a", time.Now().Add(10*time.Second), "down")
	if _, why, ok := d.Down("ws://a"); !ok || why != "down" {
		t.Fatalf("a record over a broken file did not keep: %q, %v", why, ok)
	}
	if os.Geteuid() != 0 {
		locked := relaysDown{path: filepath.Join(dir, "locked", "relays-down.json")}
		if err := os.MkdirAll(filepath.Dir(locked.path), 0o500); err != nil {
			t.Fatal(err)
		}
		locked.MarkDown("ws://a", time.Now().Add(10*time.Second), "down")
		if _, _, ok := locked.Down("ws://a"); ok {
			t.Error("a folder that arc cannot write holds a record")
		}
	}
}

// Many processes that mark relays down at once never leave a file that
// does not parse. A record ends by the wall clock.
func TestDownRelaysOfManyProcessesKeepAWholeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relays-down.json")
	var wg sync.WaitGroup
	var torn sync.Map
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if body, err := os.ReadFile(path); err == nil && !json.Valid(body) {
					torn.Store(string(body), true)
				}
			}
		})
	}
	var writers sync.WaitGroup
	for i := range 20 {
		writers.Go(func() {
			d := relaysDown{path: path}
			for j := range 20 {
				d.MarkDown(fmt.Sprintf("ws://%d-%d", i, j), time.Now().Add(10*time.Second), "down")
				d.Up(fmt.Sprintf("ws://%d-%d", i, j-1))
			}
		})
	}
	writers.Wait()
	close(stop)
	wg.Wait()
	torn.Range(func(body, _ any) bool {
		t.Errorf("a reader saw a file that does not parse: %q", body)
		return false
	})
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]downRecord
	if err := json.Unmarshal(body, &all); err != nil {
		t.Fatalf("the file does not parse after many writers: %v\n%s", err, body)
	}
	d := relaysDown{path: path}
	d.MarkDown("ws://old", time.Now().Add(-time.Second), "down")
	if _, ok := d.read()["ws://old"]; ok {
		t.Error("a record whose time passed stays in the file")
	}
}

// The relays that one command marks at once all land in the file: one
// command that found two silent relays kept one, so the next command
// waited for the other again (#444).
func TestTheMarksOfOneCommandAllLand(t *testing.T) {
	d := relaysDown{path: filepath.Join(t.TempDir(), "relays-down.json")}
	const relays = 20
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range relays {
		wg.Go(func() {
			<-start
			d.MarkDown(fmt.Sprintf("ws://relay-%d", i), time.Now().Add(10*time.Second), "no answer")
		})
	}
	close(start)
	wg.Wait()
	if got := len(d.read()); got != relays {
		t.Errorf("%d relays marked at once left %d marks, want each one", relays, got)
	}
}

// A file that arc cannot write says so, with its path and what to do, so
// that a command that waits again does not wait without a reason (#444).
func TestAFileThatCannotBeWrittenGivesANote(t *testing.T) {
	dir := t.TempDir()
	var notes []error
	d := relaysDown{path: filepath.Join(dir, "relays-down.json"), note: func(err error) { notes = append(notes, err) }}
	if err := os.Mkdir(d.path, 0o700); err != nil {
		t.Fatal(err)
	}
	d.MarkDown("ws://a", time.Now().Add(10*time.Second), "no answer")
	if len(notes) != 1 || !strings.Contains(notes[0].Error(), d.path) || !strings.Contains(notes[0].Error(), "remove it") {
		t.Errorf("a folder at the file gave the notes %v, want one that names it and says to remove it", notes)
	}
}

// A relay that an earlier command marked is skipped: its line says so, and
// when arc tries it again, not that it syncs at the next sync (#444).
func TestASkippedRelaySaysWhenArcTriesItAgain(t *testing.T) {
	err := &relay.DownError{URL: "ws://a", Why: "no answer", Until: time.Now().Add(20 * time.Second)}
	got := UnreachedLine("ws://a", err)
	if !strings.Contains(got, "ws://a: skipped: no answer at the last try; arc tries it again in 2") || strings.Contains(got, "next sync") {
		t.Errorf("a skipped relay has the line %q, want that it is skipped and when arc tries it again", got)
	}
}

// A mark further ahead than the window of a mark did not come from arc,
// whose marks end relay.DownFor after they are made. It is no mark, so a
// clock that was wrong once, or an edited file, does not skip a relay for
// ever (#444).
func TestAMarkFarAheadIsNoMark(t *testing.T) {
	d := relaysDown{path: filepath.Join(t.TempDir(), "relays-down.json"), snap: &downSnapshot{}}
	body, err := json.Marshal(map[string]downRecord{
		"ws://far":  {Until: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).Unix(), Why: "no answer"},
		"ws://near": {Until: time.Now().Add(10 * time.Second).Unix(), Why: "no answer"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := d.Down("ws://far"); ok {
		t.Error("a mark of the year 2100 counts")
	}
	if _, _, ok := d.Down("ws://near"); !ok {
		t.Error("a mark 10 s ahead does not count")
	}
}

// A broken pipe while arc writes its request is a connection that the host
// closed at once, and the line of sync says so (#444).
func TestABrokenPipeIsAClosedConnection(t *testing.T) {
	err := &net.OpError{Op: "write", Net: "tcp", Err: os.NewSyscallError("write", syscall.EPIPE)}
	if got := UnreachedReason(err); got != "the connection was closed" {
		t.Errorf("a broken pipe has the reason %q, want the connection was closed", got)
	}
}

// A step of sync says nothing about a relay that was skipped for a close
// at once in this process: the line of sync for the relay says it. The
// skip holds the close, which holds the joined dial errors, and on Linux
// one of them is only text ("server closed idle connection"); Unusable
// walked down to it, and the step printed raw lines (#444, CI run
// 40347034388).
func TestASkipForACloseAtOnceIsUnusable(t *testing.T) {
	dial := errors.Join(
		errors.New(`Get "http://127.0.0.1:1": http: server closed idle connection`),
		&net.AddrError{Err: "no suitable address found", Addr: "127.0.0.1"},
	)
	closed := &relay.ClosedError{Err: fmt.Errorf("relay ws://127.0.0.1:1: %w", dial)}
	skipped := &relay.DownError{URL: "ws://127.0.0.1:1", Why: "the connection was closed", Until: time.Now().Add(5 * time.Second), Err: closed}
	err := fmt.Errorf("the import of the drafts on the relays is not complete: no relay answered: %w", errors.Join(skipped))
	if !Unusable(err) {
		t.Errorf("a pull that a skip for a close at once ended is not Unusable: %v", err)
	}
}
