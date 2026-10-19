package iface

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/internal/testutil"
)

// countKind counts the events of one kind in the store of c.
func countKind(c *citizen, kind nostr.Kind) int {
	return len(testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{kind}})))
}

// longBody is a page of about n parts.
func longBody(n int) string {
	var body strings.Builder
	body.WriteString(pageHeader)
	line := strings.Repeat("z", 99) + "\n"
	for body.Len() < n*draft.PartSize {
		body.WriteString(line)
	}
	return body.String()
}

// A page whose draft is larger than the store keeps in one event is
// refused before arc saves any of its parts, and the error says the size.
// An older page at the address stays as it was (Cato's test of v0.36.0,
// 2026-10-19: a write of 4.04 MB failed after its parts were saved, and
// the next sync uploaded 124 parts that nothing named).
func TestAPageTooLargeForTheStoreLeavesNoPart(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", pageHeader+"the old page\n", "write", page)

	_, err := c.run("journal", longBody(125), "write", page)
	if err == nil {
		t.Fatal("a page of 125 parts was written; its draft cannot fit in one event of the store")
	}
	if !strings.Contains(err.Error(), "MB") || !strings.Contains(err.Error(), "split") {
		t.Errorf("the error does not give the size and what to do: %v", err)
	}
	if n := countKind(c, draft.PartKind); n != 0 {
		t.Errorf("the refused page left %d parts in the store, want 0", n)
	}
	if got := c.must("journal", "", "read", page); got != pageHeader+"the old page\n" {
		t.Errorf("the old page reads %q after the refused write", got)
	}
}

// Parts and checkpoints that no draft names, as a write that failed half
// way left them, go with a delete of their address, so the next sync gives
// the storage citizens a deletion for them.
func TestADeleteRemovesPartsThatNoDraftNames(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", longBody(3), "write", page)
	if n := countKind(c, draft.PartKind); n != 3 {
		t.Fatalf("the page has %d parts, want 3", n)
	}
	// Take the draft away, and leave its parts and checkpoint, as a write
	// whose draft the store refused.
	wraps := testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"k": {"30023"}}}))
	for _, wrap := range wraps {
		request := nostr.Event{Kind: 5, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", wrap.ID.Hex()}}}
		if err := request.Sign(c.env.me); err != nil {
			t.Fatal(err)
		}
		if _, err := c.env.store.Save(request); err != nil {
			t.Fatal(err)
		}
	}
	if got := c.must("journal", "", "read", page); got != "" {
		t.Fatalf("the page without its draft reads %q", got)
	}

	if _, err := c.run("journal", "", "delete", page); err != nil {
		t.Fatalf("the delete of parts that no draft names failed: %v", err)
	}
	if n := countKind(c, draft.PartKind); n != 0 {
		t.Errorf("%d parts stay after the delete, want 0", n)
	}
	if n := countKind(c, draft.CheckpointKind); n != 0 {
		t.Errorf("%d checkpoints stay after the delete, want 0", n)
	}
	if _, err := c.run("journal", "", "delete", page); err == nil {
		t.Error("a second delete found something to delete")
	}
}
