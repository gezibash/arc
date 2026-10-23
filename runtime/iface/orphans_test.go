package iface

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
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
// Another page stays as it was (Cato's test of v0.36.0, 2026-10-19: a
// write of 4.04 MB failed after its parts were saved, and the next sync
// uploaded 124 parts that nothing named).
func TestAPageTooLargeForTheStoreLeavesNoPart(t *testing.T) {
	c := newCitizen(t)
	other := "hrs/ablations/2"
	c.must("journal", "the old page\n", "write", other, "--title", "Old")
	before := c.must("journal", "", "read", other)

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
	if got := c.must("journal", "", "read", other); got != before {
		t.Errorf("the old page reads %q after the refused write", got)
	}
	if _, err := c.run("journal", "", "read", page); err == nil {
		t.Error("the refused page reads")
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
	if out, err := c.run("journal", "", "read", page); err == nil {
		t.Fatalf("the page without its draft reads %q", out)
	}

	var errs bytes.Buffer
	err := Run(context.Background(), c.env, Installed{Manifest: specManifests(t)["journal"], Author: c.author, Name: "journal"},
		[]string{"delete", page}, Stdio{In: strings.NewReader(""), Out: &bytes.Buffer{}, Err: &errs})
	if err != nil {
		t.Fatalf("the delete of parts that no draft names failed: %v", err)
	}
	if !strings.Contains(errs.String(), "removed 3 parts and 1 checkpoint that no page names") || !strings.Contains(errs.String(), "arc sync --repair") {
		t.Errorf("the delete does not say what it removed and what frees the storage: %q", errs.String())
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

// runErr runs a command, and returns its standard output and error.
func (c *citizen) runErr(id string, words ...string) (string, string, error) {
	c.t.Helper()
	var out, errs bytes.Buffer
	err := Run(context.Background(), c.env, Installed{Manifest: specManifests(c.t)[id], Author: c.author, Name: id}, words,
		Stdio{In: strings.NewReader(""), Out: &out, Err: &errs})
	return out.String(), errs.String(), err
}

// takeDraftsAway deletes the drafts of the journal and leaves their parts
// and checkpoints, as a write whose draft the store refused.
func takeDraftsAway(t *testing.T, c *citizen) {
	t.Helper()
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
}

// A write that fails after the store kept its parts names the address and
// the command that removes them (#381: Cato's write to arc/cato/90 failed,
// and he could not find which address to delete).
func TestAFailedWriteNamesTheAddressOfItsParts(t *testing.T) {
	c := newCitizen(t)
	c.env.refuse = draft.Kind
	_, err := c.run("journal", longBody(3), "write", page)
	if err == nil {
		t.Fatal("the write succeeded, but the store refused its draft")
	}
	if !strings.Contains(err.Error(), "3 parts at "+page+" that no page names") || !strings.Contains(err.Error(), "arc journal delete "+page+" removes them") {
		t.Errorf("the error does not name the address and the delete: %v", err)
	}
	if strings.Contains(err.Error(), "with the page") {
		t.Errorf("the error speaks of a page that does not exist: %v", err)
	}
	c.env.refuse = 0

	// ls cannot name parts that no checkpoint names, but it counts them.
	_, errs, err := c.runErr("journal", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs, "3 parts that no page names are at addresses that arc cannot name") {
		t.Errorf("ls does not count the parts of the failed write: %q", errs)
	}

	_, errs, err = c.runErr("journal", "delete", page)
	if err != nil {
		t.Fatalf("the delete that the error names failed: %v", err)
	}
	if !strings.Contains(errs, "removed 3 parts that no page names") {
		t.Errorf("the delete does not say what it removed: %q", errs)
	}
	if n := countKind(c, draft.PartKind); n != 0 {
		t.Errorf("%d parts stay after the delete, want 0", n)
	}
}

// An address that holds a checkpoint and parts but no draft shows in ls,
// with the command that removes them (#381).
func TestLsNamesAnAddressWithPartsAndNoPage(t *testing.T) {
	c := newCitizen(t)
	c.must("journal", longBody(3), "write", page)
	c.must("journal", strings.Replace(pageHeader, "page: 1", "page: 2", 1)+"another page\n", "write", "hrs/ablations/2")
	takeDraftsAway(t, c)
	c.must("journal", strings.Replace(pageHeader, "page: 1", "page: 3", 1)+"a page that stays\n", "write", "hrs/ablations/3")

	out, errs, err := c.runErr("journal", "ls")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "hrs/ablations/3") || strings.Contains(out, page) {
		t.Errorf("ls lists %q, want only the page that stays", out)
	}
	want := page + " holds 3 parts and 1 checkpoint that no page names, as a write that failed leaves them; arc journal delete " + page + " removes them"
	if !strings.Contains(errs, want) {
		t.Errorf("ls does not name the address:\n got %q\nwant %q", errs, want)
	}
	if !strings.Contains(errs, "hrs/ablations/2 holds 1 checkpoint that no page names") {
		t.Errorf("ls does not name the address with only a checkpoint: %q", errs)
	}
	if strings.Contains(errs, "hrs/ablations/3") {
		t.Errorf("ls names a page that has its draft: %q", errs)
	}
}

// A delete of a page whose address also holds parts that no draft names
// says how many it removed, in the words of the delete with no page (#385).
func TestADeleteOfAPageCountsItsUnnamedParts(t *testing.T) {
	c := newCitizen(t)
	c.env.refuse = draft.Kind
	if _, err := c.run("journal", longBody(3)+"more\n", "write", page); err == nil {
		t.Fatal("the write succeeded, but the store refused its draft")
	} else if !strings.Contains(err.Error(), "arc journal delete "+page+" removes them") {
		t.Errorf("the error does not name the delete: %v", err)
	}
	// A write again at the address makes the page; the parts of the
	// failed write stay beside it.
	c.env.refuse = 0
	c.must("journal", longBody(2), "write", page)
	if got := c.must("journal", "", "read", page); got != longBody(2) {
		t.Fatal("the page after the refused write reads wrong")
	}

	_, errs, err := c.runErr("journal", "delete", page)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errs, "removed 3 parts that no page names") {
		t.Errorf("the delete does not count the parts of the failed write: %q", errs)
	}
	if n := countKind(c, draft.PartKind); n != 0 {
		t.Errorf("%d parts stay after the delete, want 0", n)
	}

	// A page with no such parts prints no count.
	c.must("journal", longBody(2), "write", page)
	if _, errs, _ = c.runErr("journal", "delete", page); errs != "" {
		t.Errorf("the delete of a page with all its parts named says %q", errs)
	}
}

// ls of the journal does not report what another app left: its checkpoints
// and parts do not open with the key of the journal.
func TestLsLeavesOutWhatAnotherAppLeft(t *testing.T) {
	c := newCitizen(t)
	in := filepath.Join(t.TempDir(), "notes.txt")
	if err := os.WriteFile(in, []byte("not a page"), 0o600); err != nil {
		t.Fatal(err)
	}
	c.must("files", "", "put", in)
	wraps := testutil.Must(c.env.store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Tags: nostr.TagMap{"k": {"1063"}}}))
	for _, wrap := range wraps {
		request := nostr.Event{Kind: 5, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"e", wrap.ID.Hex()}}}
		if err := request.Sign(c.env.me); err != nil {
			t.Fatal(err)
		}
		if _, err := c.env.store.Save(request); err != nil {
			t.Fatal(err)
		}
	}
	if n := countKind(c, draft.CheckpointKind); n == 0 {
		t.Fatal("the file left no checkpoint, so this test checks nothing")
	}
	if _, errs, err := c.runErr("journal", "ls"); err != nil || errs != "" {
		t.Errorf("journal ls reports what the files app left: %q, %v", errs, err)
	}
}
