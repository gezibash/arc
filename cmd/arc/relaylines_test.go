package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/terminal"
)

// Each line of sync is escaped exactly once, also for a relay name and the
// reason of a mark that are hostile: a mark in relays-down.json is text
// that any process of the home, or a person, wrote (#444, #464). The first
// line is printed, and the last is the error of the sync, which errorLine
// prints.
func TestEachLineOfSyncIsEscapedOnce(t *testing.T) {
	name := "ws://relay\x1b[31m\u202e\n.example"
	why := "no answer\x1b[2K\u202e\nforged line"
	skipped := citizen.UnreachedLine(name, &relay.DownError{URL: name, Why: why, Until: time.Now().Add(20400 * time.Millisecond)})
	closed := citizen.UnreachedLine(name, &relay.ClosedError{Err: io.EOF})
	var out bytes.Buffer
	err := relayLines(&out, []string{skipped, closed}, false)
	wantSkipped := "arc: " + terminal.EscapeLine(name+": skipped: "+why+" at the last try; arc tries it again in 20s") + "\n"
	wantClosed := "arc: " + terminal.EscapeLine(name+": the connection was closed; arc syncs with it again at the next sync")
	if out.String() != wantSkipped {
		t.Errorf("the skipped line is %q, want %q", out.String(), wantSkipped)
	}
	if err == nil || errorLine(err) != wantClosed {
		t.Errorf("the closed line is %v, want %q", err, wantClosed)
	}
}
