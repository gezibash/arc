package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/apps/dm/cmd/arc-dm/internal/dmtest"
	"github.com/gezibash/arc/sdk/arcprogram"
)

// The short id is six hex digits, and longer where two ids of this machine
// share their start. A prefix that names two messages is refused. A real
// collision of six digits costs about 2^24 hashes, too slow for a test run
// through the relay, so this test builds the store.
func TestShortIDs(t *testing.T) {
	t.Parallel()
	a := "abcdef01" + strings.Repeat("0", 56)
	b := "abcdef02" + strings.Repeat("0", 56)
	c := "123456" + strings.Repeat("0", 58)
	s := index([]conversation{{Peers: []string{"k"}, Entries: []entry{{ID: a}, {ID: b}, {ID: c}}}})
	for id, want := range map[string]string{a: "abcdef01", b: "abcdef02", c: "123456", strings.Repeat("9", 64): "999999"} {
		if got := s.short(id); got != want {
			t.Errorf("the short id of %s is %q, want %q", id[:8], got, want)
		}
	}
	if _, err := s.lookup("abcd"); err == nil || !strings.Contains(err.Error(), `"abcd" names 2 messages`) {
		t.Errorf("an ambiguous prefix: %v", err)
	}
	if got, err := s.lookup("abcdef02"); err != nil || got.ID != b {
		t.Errorf("lookup abcdef02 is %v, %v", got.ID, err)
	}
}

// growth is how many times longer f takes for 4n items than for n, the
// least of three tries. Time that grows with n log n gives about 4.5, and
// time that grows with the square of n gives 16.
func growth(n int, f func(n int)) float64 {
	measure := func(n int) time.Duration {
		least := time.Duration(1 << 62)
		for range 3 {
			start := time.Now()
			f(n)
			least = min(least, time.Since(start))
		}
		return least
	}
	return float64(measure(4*n)) / float64(measure(n))
}

// ids makes n message ids, out of order.
func ids(n int) []entry {
	return idsFrom(0, n)
}

// idsFrom makes n message ids that a hash makes from first, first+1, and
// on: like the ids of real messages, they share no start.
func idsFrom(first, n int) []entry {
	entries := make([]entry, n)
	for i := range entries {
		sum := sha256.Sum256([]byte(fmt.Sprint(first + i)))
		entries[i].ID = hex.EncodeToString(sum[:])
	}
	return entries
}

// The store adds a batch of messages, such as the outbox, with one sort,
// so a watch that reads a large outbox stays fast.
func TestAddIsNotQuadratic(t *testing.T) {
	if g := growth(25000, func(n int) {
		s := index(nil)
		for _, e := range ids(n) {
			s.add(e, conversation{})
		}
		s.short(s.ids[0])
	}); g > 9 {
		t.Errorf("4 times the messages took %.1f times as long, want under 9", g)
	}
}

// The index of 200,000 messages takes one sort, not an insert for each
// id, whose time grows with the square of the number of messages.
func TestIndexIsNotQuadratic(t *testing.T) {
	if g := growth(25000, func(n int) { index([]conversation{{Peers: []string{"k"}, Entries: ids(n)}}) }); g > 9 {
		t.Errorf("4 times the messages took %.1f times as long, want under 9", g)
	}
	s := index([]conversation{{Peers: []string{"k"}, Entries: ids(1000)}})
	if sorted := s.sorted(); len(sorted) != 1000 || !slices.IsSorted(sorted) {
		t.Errorf("the index holds %d ids, sorted %v", len(sorted), slices.IsSorted(sorted))
	}
}

// A watch adds one message to its store in its place, with no sort: at
// 100,000 messages, a sort for each message took 2 to 3 percent of the
// time of a whole index, and an insert takes about 0.1 percent.
func TestALiveAddDoesNotSortTheStore(t *testing.T) {
	extra := idsFrom(1<<30, 200)
	ratio := 1.0
	for range 3 {
		start := time.Now()
		index([]conversation{{Peers: []string{"k"}, Entries: ids(100000)}})
		whole := time.Since(start)
		s := index([]conversation{{Peers: []string{"k"}, Entries: ids(100000)}})
		start = time.Now()
		for _, e := range extra {
			s.add(e, conversation{})
			s.short(e.ID)
		}
		ratio = min(ratio, float64(time.Since(start))/float64(len(extra))/float64(whole))
	}
	if ratio > 0.006 {
		t.Errorf("one message took %.4f of the time of a whole index, want under 0.006", ratio)
	}
}

// testWatcher is a watcher with an outbox that the test fills, and that
// records what it prints.
func testWatcher(outbox ...arcprogram.Sent) (w *watcher, printed *[]string, reads *int) {
	w, printed, reads, _ = testWatcherWith(outbox...)
	return w, printed, reads
}

// testWatcherWith is testWatcher, with the outbox that a test can change.
func testWatcherWith(outbox ...arcprogram.Sent) (w *watcher, printed *[]string, reads *int, sent *[]arcprogram.Sent) {
	printed, reads, sent = &[]string{}, new(int), &outbox
	w = &watcher{
		outbox: func(context.Context) ([]arcprogram.Sent, error) {
			*reads++
			return *sent, nil
		},
		me:       "me",
		messages: index(nil),
		missed:   map[string]bool{},
		bySender: map[string][]int{},
		last:     time.Now(),
	}
	w.print = func(m arcprogram.Message, messages *store, quote bool) {
		e := entry{ID: m.ID, Text: m.Text, ReplyTo: m.ReplyTo}
		if !quote {
			*printed = append(*printed, m.Text+" (plain)")
			return
		}
		*printed = append(*printed, m.Text+" "+messages.quote(e, conversation{Peers: []string{"peer"}}))
	}
	return w, printed, reads, sent
}

// msg is a message of the tests of the watcher: one per id, from a sender.
func msg(id int, from, text, replyTo string) arcprogram.Message {
	return arcprogram.Message{ID: idsFrom(id, 1)[0].ID, Text: text, ReplyTo: replyTo, From: from, Members: []string{from, "me"}}
}

// sentParent is a message of this citizen in the outbox, to "peer".
func sentParent(id, text string) arcprogram.Sent {
	return arcprogram.Sent{ID: id, To: "peer", Text: text, Members: []string{"me", "peer"}}
}

// inOrder reports whether the printed texts of each sender, named by the
// first word of the text, come in the order of their numbers.
func inOrder(printed []string) bool {
	last := map[string]int{}
	for _, line := range printed {
		var who string
		var n int
		if _, err := fmt.Sscanf(line, "%s %d", &who, &n); err != nil {
			continue
		}
		if prev, ok := last[who]; ok && n <= prev {
			return false
		}
		last[who] = n
	}
	return true
}

// quoted counts the printed lines that hold the quote of the parent.
func quoted(printed []string) int {
	n := 0
	for _, line := range printed {
		if strings.HasSuffix(line, "\u21b3 ababab  you: bring the map") {
			n++
		}
	}
	return n
}

// The watch holds at most maxQueue messages, and loses none: 100 senders
// that each send many messages behind a reply that waits.
func TestTheHeldQueueIsCapped(t *testing.T) {
	w, printed, _ := testWatcher()
	w.take(msg(0, "peer", "peer 0", strings.Repeat("7a", 32)))
	for i := range 3 * maxQueue {
		w.take(msg(i+1, fmt.Sprint("s", i%100), fmt.Sprintf("s%d %d", i%100, i), ""))
		if w.live > maxQueue {
			t.Fatalf("the watch holds %d messages, want at most %d", w.live, maxQueue)
		}
	}
	w.close()
	if len(*printed) != 3*maxQueue+1 || !inOrder(*printed) {
		t.Errorf("the watch printed %d messages, in order %v; want %d in the order of each sender", len(*printed), inOrder(*printed), 3*maxQueue+1)
	}
}

// Cato's case: one junk reply, one valid reply whose parent is in the
// outbox, then 254 junk replies of the same sender. The junk past the
// quota prints at once, oldest first, and the valid reply gets its quote.
func TestOneSendersJunkDoesNotHideAValidReply(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, reads := testWatcher(sentParent(parent, "bring the map"))
	w.take(msg(0, "mallory", "mallory 0", strings.Repeat("7a", 32)))
	w.take(msg(1, "peer", "peer 1", parent))
	for i := range 254 {
		w.take(msg(i+2, "mallory", fmt.Sprintf("mallory %d", i+1), idsFrom(1000+i, 1)[0].ID))
	}
	w.close()
	if quoted(*printed) != 1 || *reads == 0 || len(*printed) != 256 || !inOrder(*printed) {
		t.Errorf("after %d reads the watch printed %d messages, %d quoted, in order %v", *reads, len(*printed), quoted(*printed), inOrder(*printed))
	}
}

// Cato's case with keys: 16 keys take turns to send 255 junk replies, after
// one junk reply and one valid one, within 1 s of a read. No valid reply
// is hidden: past the cap, the watch reads the outbox a second after the
// last read, and no waiting reply shows before it.
func TestRotatingKeysDoNotHideAValidReply(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _ := testWatcher(sentParent(parent, "bring the map"))
	w.take(msg(0, "k0", "k0 0", strings.Repeat("7a", 32)))
	w.take(msg(1, "peer", "peer 1", parent))
	for i := range 255 {
		key := fmt.Sprint("k", i%16)
		w.take(msg(i+2, key, fmt.Sprintf("%s %d", key, i+1), idsFrom(1000+i, 1)[0].ID))
	}
	w.close()
	if quoted(*printed) != 1 || len(*printed) != 257 || !inOrder(*printed) {
		t.Errorf("the watch printed %d messages, %d quoted, in order %v", len(*printed), quoted(*printed), inOrder(*printed))
	}
}

// One honest sender replies 20 times, quickly, to messages that this
// citizen sent: each reply waits for the read, gets its quote, and keeps
// its place.
func TestQuickHonestRepliesKeepQuotesAndOrder(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _ := testWatcher(sentParent(parent, "bring the map"))
	for i := range 20 {
		w.take(msg(i, "peer", fmt.Sprintf("peer %d", i), parent))
	}
	w.close()
	if quoted(*printed) != 20 || !inOrder(*printed) {
		t.Errorf("the watch printed %q, want 20 quoted replies in order", *printed)
	}
}

// A sender that holds more than maxHeldPerSender prints its oldest at once,
// in its own order, and the rest wait behind them.
func TestASenderOverItsQuotaPrintsItsOldest(t *testing.T) {
	w, printed, _ := testWatcher()
	defer w.close()
	for i := range 100 {
		w.take(msg(i, "mallory", fmt.Sprintf("mallory %d", i), idsFrom(1000+i, 1)[0].ID))
	}
	if len(*printed) != 100-maxHeldPerSender || w.live != maxHeldPerSender || !inOrder(*printed) || !strings.HasPrefix((*printed)[0], "mallory 0 ") {
		t.Errorf("the watch printed %d and holds %d, want %d printed, oldest first, and %d held", len(*printed), w.live, 100-maxHeldPerSender, maxHeldPerSender)
	}
}

// A busy group, 600 messages behind a valid reply that waits, does not
// push the reply out unquoted, and each sender keeps its order.
func TestABusyGroupDoesNotHideAValidReply(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _ := testWatcher(sentParent(parent, "bring the map"))
	w.take(msg(0, "peer", "peer 0", parent))
	for i := range 600 {
		who := fmt.Sprint("m", i%3)
		w.take(msg(i+1, who, fmt.Sprintf("%s %d", who, i), ""))
	}
	w.close()
	if len(*printed) != 601 || quoted(*printed) != 1 || !inOrder(*printed) {
		t.Errorf("the watch printed %d messages, %d quoted, in order %v", len(*printed), quoted(*printed), inOrder(*printed))
	}
}

// Past maxWaiting replies that wait, with the read blocked, no reply shows
// early: the watch reads the outbox when overflowRead has passed, and each
// reply gets its quote. 128 senders hold two each.
func TestSmallHoldersWaitForTheRead(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, reads := testWatcher(sentParent(parent, "bring the map"))
	defer w.close()
	for i := range maxWaiting {
		who := fmt.Sprint("s", i/2)
		w.take(msg(i, who, fmt.Sprintf("%s %d", who, i), parent))
	}
	w.mu.Lock()
	early := len(*printed)
	w.mu.Unlock()
	time.Sleep(overflowRead + 500*time.Millisecond)
	w.mu.Lock()
	defer w.mu.Unlock()
	if early != 0 || *reads != 1 || quoted(*printed) != maxWaiting {
		t.Errorf("%d printed before the read; after %d reads, %d of %d replies are quoted", early, *reads, quoted(*printed), maxWaiting)
	}
}

// At the hard cap, with each held message waiting and the read blocked,
// the watch waits for the read without its lock: the timer and the end of
// the watch can run meanwhile.
func TestTheWaitAtTheCapHoldsNoLock(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, _, _ := testWatcher(sentParent(parent, "bring the map"))
	defer w.close()
	for i := range maxQueue - 1 {
		who := fmt.Sprint("s", i%(maxQueue/maxHeldPerSender))
		w.take(msg(i, who, fmt.Sprintf("%s %d", who, i), parent))
	}
	go w.take(msg(maxQueue, "last", "last 0", parent))
	time.Sleep(100 * time.Millisecond)
	free := false
	for deadline := time.Now().Add(overflowRead / 2); time.Now().Before(deadline) && !free; time.Sleep(10 * time.Millisecond) {
		if free = w.mu.TryLock(); free {
			w.mu.Unlock()
		}
	}
	if !free {
		t.Error("the watch held its lock while it waited for the read")
	}
}

// At the hard cap with the read blocked, only plain messages that lead
// their sender's held messages show early; a reply that waits, and each
// message behind it, wait for the read.
func TestOnlyPlainHeadsShowEarly(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _ := testWatcher(sentParent(parent, "bring the map"))
	defer w.close()
	w.take(msg(0, "peer", "peer 0", parent))
	w.take(msg(1, "peer", "peer 1", ""))
	for i := range maxQueue {
		who := fmt.Sprint("s", i%100)
		w.take(msg(i+2, who, fmt.Sprintf("%s %d", who, i), ""))
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, line := range *printed {
		if strings.HasPrefix(line, "peer ") {
			t.Fatalf("a message of the sender that waits showed before the read: %q", line)
		}
	}
	if len(*printed) == 0 {
		t.Error("no plain message showed early")
	}
}

// A read that fails while the watch runs shows the replies that wait with
// no quote line, and marks no parent missed: their parents can be on this
// device. A later reply to the same parent, after a read that works, gets
// its quote.
func TestAFailedReadMarksNothingMissed(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _, sent := testWatcherWith()
	read := w.outbox
	fail := true
	w.outbox = func(ctx context.Context) ([]arcprogram.Sent, error) {
		if fail {
			return nil, errors.New("arc failed")
		}
		return read(ctx)
	}
	w.last = time.Now().Add(-refreshEvery)
	w.take(msg(0, "peer", "peer 0", parent))
	if len(*printed) != 1 || (*printed)[0] != "peer 0 (plain)" || len(w.messages.shown) != 0 {
		t.Fatalf("after a failed read the watch printed %q and showed %d ids as missing", *printed, len(w.messages.shown))
	}
	fail = false
	*sent = append(*sent, sentParent(parent, "bring the map"))
	w.take(msg(1, "peer", "peer 1", parent))
	w.close()
	if last := (*printed)[len(*printed)-1]; last != "peer 1 \u21b3 ababab  you: bring the map" {
		t.Errorf("the reply after a read that worked printed %q", last)
	}
}

// Cato's case: 300 keys send one junk reply each, within 1 s of a read,
// while an honest sender has 30 replies that wait. The honest replies all
// get their quotes: only plain messages and senders over their quota show
// early.
func TestManyKeysDoNotHideAnHonestSender(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _ := testWatcher(sentParent(parent, "bring the map"))
	for i := range 30 {
		w.take(msg(i, "peer", fmt.Sprintf("peer %d", i), parent))
	}
	for i := range 300 {
		key := fmt.Sprint("k", i)
		w.take(msg(1000+i, key, key+" 0", idsFrom(5000+i, 1)[0].ID))
	}
	w.close()
	if quoted(*printed) != 30 || len(*printed) != 330 || !inOrder(*printed) {
		t.Errorf("the watch printed %d messages, %d of 30 quoted, in order %v", len(*printed), quoted(*printed), inOrder(*printed))
	}
}

// A reply that shows early because its sender is over the quota marks
// its parent not missed: a later reply to it, once the parent is in the
// outbox, gets its quote.
func TestAnEarlyReplyMarksNoParentMissed(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, _, sent := testWatcherWith()
	for i := range maxHeldPerSender + 6 {
		w.take(msg(i, "mallory", fmt.Sprintf("mallory %d", i), parent))
	}
	if len(*printed) != 6 {
		t.Fatalf("%d replies showed early, want 6", len(*printed))
	}
	*sent = append(*sent, sentParent(parent, "bring the map"))
	w.take(msg(99999, "peer", "peer 1", parent))
	w.close()
	if quoted(*printed) != maxHeldPerSender+1 {
		t.Errorf("%d replies are quoted, want the %d that waited", quoted(*printed), maxHeldPerSender+1)
	}
}

// An overflow reads the outbox when the last read is older than
// overflowRead, so the replies that wait get their quotes.
func TestAnOverflowReadsAnOldOutbox(t *testing.T) {
	parent := strings.Repeat("ab", 32)
	w, printed, reads := testWatcher(sentParent(parent, "bring the map"))
	w.last = time.Now().Add(-2 * overflowRead)
	defer w.close()
	for i := range maxWaiting {
		who := fmt.Sprint("s", i%16)
		w.take(msg(i, who, fmt.Sprintf("%s %d", who, i), parent))
	}
	if *reads != 1 || quoted(*printed) != maxWaiting {
		t.Errorf("after %d reads, %d of %d replies are quoted", *reads, quoted(*printed), maxWaiting)
	}
}

// pairs makes 2n ids in pairs that share their first 10 digits and differ
// at the 11th, as a
// sender who spent 2^40 hashes on each pair could make them.
func pairs(n int) []entry {
	var out []entry
	for _, e := range idsFrom(1<<20, n) {
		// The twin differs at the 11th digit.
		eleventh := map[bool]string{true: "1", false: "0"}[e.ID[10] == '0']
		twin := e.ID[:10] + eleventh + idsFrom(1<<21+len(out), 1)[0].ID[11:]
		out = append(out, e, entry{ID: twin})
	}
	return out
}

// Ids that share 8 digits take their short id from their bucket: it is
// long enough to name one message, and a flood of such pairs stays linear.
func TestCraftedCollisionsStayFast(t *testing.T) {
	s := index([]conversation{{Peers: []string{"k"}, Entries: pairs(10)}})
	for _, e := range pairs(10) {
		if short := s.short(e.ID); len(short) != 11 || short != e.ID[:11] {
			t.Errorf("the short id of %s is %q, want its first 11 digits", e.ID[:12], short)
		}
	}
	if g := growth(10000, func(n int) {
		s := index(nil)
		for _, e := range pairs(n) {
			s.add(e, conversation{})
			s.short(e.ID)
		}
	}); g > 9 {
		t.Errorf("4 times the pairs took %.1f times as long, want under 9", g)
	}
}

// A watch remembers at most maxMissed ids that a read of the outbox did
// not find.
func TestTheWatchForgetsMissedIds(t *testing.T) {
	w := &watcher{missed: map[string]bool{}}
	for i := range 2 * maxMissed {
		w.miss(fmt.Sprint(i))
	}
	if len(w.missed) > maxMissed {
		t.Errorf("the watch remembers %d ids, want at most %d", len(w.missed), maxMissed)
	}
}

// A parent of only spaces, such as U+3000, quotes as (blank). arc refuses
// to send one, so no relay test can make it; another client can.
func TestABlankParentQuotesAsBlank(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"\u3000\u3000\u3000": "(blank)",
		" \n\t\n":            "(blank)",
		"\u3000\u3000\nreal": "real",
		"real\nmore":         "real \u2026",
	} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q, want %q", in, got, want)
		}
	}
}

// A short id that arc-dm showed as a parent not on this device is refused
// later: a sender can name, as a parent, an id that starts as its own
// later message.
func TestAShownMissingIdIsRefused(t *testing.T) {
	parent := idsFrom(1, 1)[0].ID
	later := parent[:12] + idsFrom(2, 1)[0].ID[12:]
	s := index([]conversation{{Peers: []string{"mallory"}, Entries: []entry{{ID: later}}}})
	s.avoid = map[string]bool{parent: true}
	if _, err := s.lookup(parent[:12]); err == nil || !strings.Contains(err.Error(), "was not on this device") {
		t.Errorf("lookup of the shown id: %v", err)
	}
	if got, err := s.lookup(later); err != nil || got.ID != later {
		t.Errorf("lookup of the full id: %v, %v", got.ID, err)
	}
	// An id that arc-dm showed as missing, and that is here now, such as a
	// message of this citizen, is not refused.
	s.add(entry{ID: parent}, conversation{Peers: []string{"mallory"}})
	if got, err := s.lookup(parent[:13]); err != nil || got.ID != parent {
		t.Errorf("lookup of a shown id that is here now: %v, %v", got.ID, err)
	}
}

// The tests of the watch in e2e wait on a copy of refreshEvery.
func TestDmtestKeepsTheTimesOfTheWatch(t *testing.T) {
	if dmtest.RefreshEvery != refreshEvery {
		t.Errorf("dmtest.RefreshEvery is %v, want %v as in main.go", dmtest.RefreshEvery, refreshEvery)
	}
}
