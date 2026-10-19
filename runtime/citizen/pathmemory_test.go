package citizen

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/runtime/selfsync"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
)

// opens is a fake storage open. Each call takes the next result, and
// records the attempts that it was asked for.
type opens struct {
	asked   []string
	results []func() (*storage.Conn, error)
}

func (o *opens) open(noRelay, onlyRelay bool) (*storage.Conn, error) {
	switch {
	case onlyRelay:
		o.asked = append(o.asked, "turn")
	case noRelay:
		o.asked = append(o.asked, "direct")
	default:
		o.asked = append(o.asked, "auto")
	}
	if len(o.results) == 0 {
		return nil, errors.New("openPipe asked for one attempt too many")
	}
	next := o.results[0]
	o.results = o.results[1:]
	return next()
}

func through(path string) func() (*storage.Conn, error) {
	return func() (*storage.Conn, error) { return &storage.Conn{Path: path}, nil }
}

func failing(err error) func() (*storage.Conn, error) {
	return func() (*storage.Conn, error) { return nil, err }
}

// pathSync runs openPipe for the storage citizen "box" at the time at.
func pathSync(t *testing.T, syncer *selfsync.Syncer, policy direct.Policy, turnOK bool, at time.Time, results ...func() (*storage.Conn, error)) ([]string, string, error) {
	t.Helper()
	o := &opens{results: results}
	var notes bytes.Buffer
	_, err := openPipe(syncer, "box", "storage-1", policy, turnOK, func() time.Time { return at }, &notes, o.open)
	if len(o.results) != 0 {
		t.Fatalf("openPipe left %d results unused after asking for %v", len(o.results), o.asked)
	}
	return o.asked, notes.String(), err
}

func sameAttempts(t *testing.T, what string, got []string, want ...string) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("%s asked for the attempts %v, want %v", what, got, want)
	}
}

// After the direct attempts to a storage citizen find no path, each sync
// for 6 hours goes to the TURN server of the relay first, and says so.
// Then a sync tries the direct attempts first again (Zim, 2026-10-19).
func TestADirectFailureSendsTheNextSyncsToTURNFor6Hours(t *testing.T) {
	syncer := &selfsync.Syncer{State: t.TempDir()}
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)

	asked, _, err := pathSync(t, syncer, direct.Auto, true, start, through("relay"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "the first sync", asked, "auto")

	asked, notes, err := pathSync(t, syncer, direct.Auto, true, start.Add(5*time.Hour+59*time.Minute), through("relay"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "a sync 5 h 59 min later", asked, "turn")
	if !strings.Contains(notes, "storage-1: the direct attempts found no path; until ") || !strings.Contains(notes, "so the bytes go through the relay") {
		t.Fatalf("a sync that goes to TURN first does not say so:\n%s", notes)
	}

	// A sync through TURN first does not move the end of the 6 hours.
	asked, _, err = pathSync(t, syncer, direct.Auto, true, start.Add(6*time.Hour), through("srflx to srflx"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "a sync 6 h later", asked, "auto")

	// The direct path opened, so the next sync tries it first.
	asked, _, err = pathSync(t, syncer, direct.Auto, true, start.Add(6*time.Hour+time.Minute), through("srflx to srflx"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "a sync after a direct path opened", asked, "auto")
}

// A sync that goes to TURN first and finds no path there tries the direct
// attempts. A direct path that opens ends the time of TURN first.
func TestNoPathThroughTURNFallsBackToTheDirectAttempts(t *testing.T) {
	syncer := &selfsync.Syncer{State: t.TempDir()}
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if _, _, err := pathSync(t, syncer, direct.Auto, true, start, through("relay")); err != nil {
		t.Fatal(err)
	}

	asked, _, err := pathSync(t, syncer, direct.Auto, true, start.Add(time.Hour), failing(direct.NoPath(direct.ErrNoPath)), through("host to host"))
	if err != nil {
		t.Fatalf("the direct attempts opened a pipe, but openPipe failed: %v", err)
	}
	sameAttempts(t, "a sync whose TURN attempt found no path", asked, "turn", "direct")

	asked, _, err = pathSync(t, syncer, direct.Auto, true, start.Add(2*time.Hour), through("host to host"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "the sync after the direct path opened", asked, "auto")
}

// A storage citizen that does not answer the call gets no second try in
// the same sync: the direct attempts would make the same call.
func TestAFailedCallGetsNoDirectAttempt(t *testing.T) {
	syncer := &selfsync.Syncer{State: t.TempDir()}
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if _, _, err := pathSync(t, syncer, direct.Auto, true, start, through("relay")); err != nil {
		t.Fatal(err)
	}
	offline := errors.New("no answer from the storage citizen")
	asked, _, err := pathSync(t, syncer, direct.Auto, true, start.Add(time.Hour), failing(callError{offline}))
	if !errors.Is(err, offline) {
		t.Fatalf("openPipe gave %v, want the error of the call", err)
	}
	sameAttempts(t, "a sync whose call failed", asked, "turn")

	// A sync that ends, such as by Ctrl-C, makes no second attempt either.
	asked, _, err = pathSync(t, syncer, direct.Auto, true, start.Add(time.Hour), failing(context.Canceled))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("openPipe gave %v, want the end of the sync", err)
	}
	sameAttempts(t, "a sync that ended", asked, "turn")
}

// A repair without --turn needs a direct pipe, and the paths turn and
// direct are the choice of the citizen: none of them goes to TURN first.
func TestTURNFirstSkipsRepairAndAFixedPath(t *testing.T) {
	syncer := &selfsync.Syncer{State: t.TempDir()}
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if _, _, err := pathSync(t, syncer, direct.Auto, true, start, through("relay")); err != nil {
		t.Fatal(err)
	}
	later := start.Add(time.Hour)
	for _, c := range []struct {
		what   string
		policy direct.Policy
		turnOK bool
		want   string
	}{
		{"a repair without --turn", direct.Auto, false, "auto"},
		{"the path direct", direct.DirectOnly, true, "direct"},
		{"the path turn", direct.RelayOnly, true, "turn"},
	} {
		asked, _, err := pathSync(t, syncer, c.policy, c.turnOK, later, through("relay"))
		if err != nil {
			t.Fatal(err)
		}
		sameAttempts(t, c.what, asked, c.want)
	}
	// The path session tries a direct path first, as auto does, so it
	// follows the record too.
	asked, _, err := pathSync(t, syncer, direct.Off, true, later, through("relay"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "the path session", asked, "turn")
}

// A clock that went back before the recorded failure ends the time of
// TURN first, so a wrong clock cannot keep a citizen on TURN.
func TestAClockThatWentBackTriesTheDirectAttempts(t *testing.T) {
	syncer := &selfsync.Syncer{State: t.TempDir()}
	start := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	if _, _, err := pathSync(t, syncer, direct.Auto, true, start, through("relay")); err != nil {
		t.Fatal(err)
	}
	asked, _, err := pathSync(t, syncer, direct.Auto, true, start.Add(-time.Minute), through("relay"))
	if err != nil {
		t.Fatal(err)
	}
	sameAttempts(t, "a sync with the clock 1 min before the failure", asked, "auto")
}
