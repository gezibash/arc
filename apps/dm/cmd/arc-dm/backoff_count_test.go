package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
)

// The watch reads the outbox only from the timer, refreshEvery after the end
// of the last read, however many replies wait. A fake outbox that takes
// readCost records each read; replies to ids that no message has arrive
// every 100 ms for the whole window. The test counts reads and measures the
// gaps between them, so load can only make it pass more slowly, never fail:
// a slower machine gives fewer reads and longer gaps, and the bound on take
// is far above any scheduling delay.
func TestTheOutboxIsReadAtMostOnceEachRefreshFromTheEndOfTheLast(t *testing.T) {
	const (
		window   = 3*refreshEvery - time.Second // 14 s
		readCost = 2 * time.Second
		takeMax  = readCost / 2
	)
	w, _, _ := testWatcher()
	var mu sync.Mutex
	type span struct{ start, end time.Time }
	var reads []span
	w.outbox = func(ctx context.Context) ([]arcprogram.Sent, error) {
		s := span{start: time.Now()}
		select {
		case <-time.After(readCost):
		case <-ctx.Done():
		}
		s.end = time.Now()
		mu.Lock()
		reads = append(reads, s)
		mu.Unlock()
		return nil, nil
	}
	w.mu.Lock()
	w.lastEnd = time.Now().Add(-refreshEvery) // the first read is due at once
	w.mu.Unlock()
	start := time.Now()
	var slowest time.Duration
	for i := 0; time.Since(start) < window; i++ {
		t0 := time.Now()
		w.take(msg(i, fmt.Sprintf("s%d", i%8), fmt.Sprintf("junk %d", i), fmt.Sprintf("%064x", i+1)))
		slowest = max(slowest, time.Since(t0))
		time.Sleep(100 * time.Millisecond)
	}
	elapsed := time.Since(start)
	w.close() // the end of the watch may read once more; it is not counted
	mu.Lock()
	defer mu.Unlock()
	var during []span
	for _, r := range reads {
		if r.start.Before(start.Add(elapsed)) {
			during = append(during, r)
		}
	}
	if slowest > takeMax {
		t.Errorf("a take waited %v, want under %v: take must not read", slowest, takeMax)
	}
	// One read at once, then one each readCost+refreshEvery at most.
	most := 1 + int(elapsed/(readCost+refreshEvery)) + 1
	if len(during) < 2 || len(during) > most {
		t.Errorf("the watch read the outbox %d times in %v, want 2 to %d", len(during), elapsed, most)
	}
	minGap := time.Duration(1 << 62)
	for i := 1; i < len(during); i++ {
		minGap = min(minGap, during[i].start.Sub(during[i-1].end))
	}
	t.Logf("reads %d in %v, smallest gap %v, slowest take %v", len(during), elapsed, minGap, slowest)
	for i := 1; i < len(during); i++ {
		if gap := during[i].start.Sub(during[i-1].end); gap < refreshEvery-100*time.Millisecond {
			t.Errorf("read %d started %v after read %d ended, want at least %v", i, gap, i-1, refreshEvery)
		}
	}
}
