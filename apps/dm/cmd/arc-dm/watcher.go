package main

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/terminal"
)

// watcher prints the messages of a watch.
//
// A reply can name a parent that this citizen sent after the watch
// started, from another process: only a read of the outbox finds it. A
// sender can name many ids that no message has, and each read of the
// outbox opens each sealed message that this citizen sent. So only a timer
// reads the outbox, refreshEvery after the end of the last read, and the
// lock of the watcher is free while it reads. A reply whose parent the
// watch does not hold waits for a read that starts after it arrives, and
// each message of its sender after it waits too. An id that a read did not
// find is not waited for again; the watch forgets such ids when it holds
// maxMissed.
//
// A waiting reply prints without its quote only after a read of the
// outbox that started after it, with two exceptions. A sender that holds
// more than maxHeldPerSender messages prints its oldest at once. While the
// last read failed or took more than readTimeout, a reply prints at once
// with no quote line, unless its sender has held messages; the next read
// shows whether reads work again. Past maxWaiting waiting replies, the
// timer reads overflowRead after the end of the last read. Past maxQueue
// held messages, the watch prints early only plain messages that lead the
// held messages of their sender, and then waits for the read, without its
// lock. The messages of one sender always print in their order; a
// sender's messages can print ahead of the messages of others.
type watcher struct {
	mu     sync.Mutex
	outbox func(context.Context) ([]arcprogram.Sent, error)
	// print shows a message; with quote false, it shows a reply with no
	// quote line at all, as after a read of the outbox that failed.
	print    func(m arcprogram.Message, messages *store, quote bool)
	me       string
	messages *store
	missed   map[string]bool
	lastEnd  time.Time // the end of the last read of the outbox
	// queue holds the held messages in the order of their arrival; an
	// entry is nil once its message printed early.
	queue []*arcprogram.Message
	// bySender holds the places in queue of each sender's held messages,
	// oldest first.
	bySender map[string][]int
	live     int         // the messages in queue that did not print
	waiting  int         // the messages in queue that wait for a read
	timer    *time.Timer // the next read, when a message waits for it
	due      time.Time   // the time of the next read
	reading  bool        // a read of the outbox runs, without the lock
	drained  *sync.Cond  // signals the end of each read
	readOK   bool        // the last read of the outbox worked
	closed   bool
	// ctx ends each read of the outbox while the watch runs: the signal
	// that ends the watch ends a read on a hung arc too. closeCtx bounds
	// the read at the end of the watch: a second signal ends it.
	ctx      context.Context
	closeCtx context.Context
	// saveShown saves the ids that the watch showed as missing; nil in
	// tests. The watch calls it after a read that showed one.
	saveShown func()
}

const (
	refreshEvery     = 5 * time.Second
	maxMissed        = 1024
	maxWaiting       = 256
	maxHeldPerSender = 64
	maxQueue         = 4096
	overflowRead     = time.Second
	// readTimeout bounds each read of the outbox while the watch runs.
	readTimeout = 10 * time.Second
	// closeRead bounds the read of the outbox at the end of a watch,
	// after the signal that ended it.
	closeRead = 10 * time.Second
)

// take prints m, or holds it until a read of the outbox. It never reads
// the outbox itself.
func (w *watcher) take(m arcprogram.Message) {
	w.mu.Lock()
	defer w.mu.Unlock()
	waits := w.waits(m)
	if waits && !w.readOK && len(w.bySender[m.From]) == 0 {
		// Reads of the outbox fail: show the reply at once, and let a
		// later read show whether they work again.
		w.print(m, w.messages, false)
		w.schedule()
		return
	}
	if w.live == 0 && !waits {
		w.print(m, w.messages, true)
		return
	}
	w.hold(m)
	for len(w.bySender[m.From]) > maxHeldPerSender {
		w.printOldest(m.From)
	}
	for w.live >= maxQueue {
		if w.printPlainHeads() {
			continue
		}
		if w.ctx != nil && w.ctx.Err() != nil {
			return // the watch ends, and its end prints what is held
		}
		// Each held message waits, or follows one that waits, of its
		// sender: wait for the read, without the lock.
		w.schedule()
		w.cond().Wait()
	}
	if w.waiting == 0 && !w.reading {
		w.flushTo(len(w.queue), false)
		return
	}
	w.schedule()
}

// hold puts m at the end of the held messages.
func (w *watcher) hold(m arcprogram.Message) {
	w.bySender[m.From] = append(w.bySender[m.From], len(w.queue))
	w.queue = append(w.queue, &m)
	w.live++
	if w.waits(m) {
		w.waiting++
	}
}

func (w *watcher) cond() *sync.Cond {
	if w.drained == nil {
		w.drained = sync.NewCond(&w.mu)
	}
	return w.drained
}

// schedule sets the timer for the next read: refreshEvery after the end of
// the last read, or overflowRead past maxWaiting waiting replies. A read
// that runs schedules the next when it ends.
func (w *watcher) schedule() {
	if w.reading || w.closed {
		return
	}
	after := refreshEvery
	if w.waiting >= maxWaiting {
		after = overflowRead
	}
	w.readIn(max(0, after-time.Since(w.lastEnd)))
}

// readIn reads the outbox and prints what is held after wait, or sooner
// if a read is due sooner.
func (w *watcher) readIn(wait time.Duration) {
	if w.timer != nil {
		if time.Until(w.due) <= wait {
			return
		}
		w.stop()
	}
	w.due = time.Now().Add(wait)
	w.timer = time.AfterFunc(wait, func() {
		w.mu.Lock()
		w.timer = nil
		if !w.reading && !w.closed {
			w.readAndFlush(w.readCtx())
		}
		save := w.saveShown
		if len(w.messages.shown) == 0 {
			save = nil
		}
		w.mu.Unlock()
		if save != nil {
			save()
		}
	})
}

// printPlainHeads prints, for each sender, the plain messages that lead
// its held messages: they wait for no read, and no message of their
// sender comes before them. It reports whether it printed any.
func (w *watcher) printPlainHeads() bool {
	printed := false
	for sender := range w.bySender {
		for places, ok := w.bySender[sender]; ok && !w.waits(*w.queue[places[0]]); places, ok = w.bySender[sender] {
			w.printOldest(sender)
			printed = true
		}
	}
	return printed
}

// printOldest prints the oldest held message of a sender, as it is: a
// reply that waits shows no quote, and its parent is not missed.
func (w *watcher) printOldest(sender string) {
	places := w.bySender[sender]
	i := places[0]
	if len(places) == 1 {
		delete(w.bySender, sender)
	} else {
		w.bySender[sender] = places[1:]
	}
	m := w.queue[i]
	w.queue[i] = nil
	w.live--
	if w.waits(*m) {
		w.waiting--
	}
	// readOK is true here today: while reads fail, nothing is held, since
	// a failed read prints all and take then holds nothing. It stays the
	// argument, so that a later path that holds messages while reads fail
	// does not show a parent as missing.
	w.print(*m, w.messages, w.readOK)
	// A read that runs prints the messages before its place in queue,
	// so the places stay until it ends.
	if !w.reading && len(w.queue) > 2*w.live+64 {
		w.compact()
	}
}

// compact drops the entries of the messages that printed early.
func (w *watcher) compact() {
	var queue []*arcprogram.Message
	clear(w.bySender)
	for _, m := range w.queue {
		if m != nil {
			w.bySender[m.From] = append(w.bySender[m.From], len(queue))
			queue = append(queue, m)
		}
	}
	w.queue = queue
}

func (w *watcher) stop() {
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// waits reports whether m names a parent that a read of the outbox could
// find.
func (w *watcher) waits(m arcprogram.Message) bool {
	_, held := w.messages.byID[m.ReplyTo]
	return m.ReplyTo != "" && !held && !w.missed[m.ReplyTo]
}

// readCtx is the context of a read of the outbox while the watch runs.
func (w *watcher) readCtx() context.Context {
	if w.ctx == nil {
		return context.Background()
	}
	return w.ctx
}

// readAndFlush reads the outbox, without the lock, and prints the messages
// that were held when the read started. It is called with the lock held.
//
// A read takes at most readTimeout. A read that the end of the watch cut
// off leaves the held messages for the end of the watch. A read that fails
// or takes longer prints each held message as it is, a reply that waits
// with no quote line, and marks no parent missed. A message that arrived
// while the read ran waits for the next read, or prints now if nothing
// that it waits for is missing.
func (w *watcher) readAndFlush(ctx context.Context) {
	w.stop()
	before := len(w.queue)
	w.reading = true
	w.mu.Unlock()
	read, cancel := context.WithTimeout(ctx, readTimeout)
	outbox, err := w.outbox(read)
	timedOut := read.Err() == context.DeadlineExceeded
	cancel()
	w.mu.Lock()
	w.reading = false
	w.lastEnd = time.Now()
	defer w.cond().Broadcast()
	switch {
	case err == nil:
		for _, sent := range sentMessages(outbox) {
			w.messages.add(sent.entry, conversation{Peers: peersOf(w.me, sent.members)})
		}
		w.readOK = true
		w.flushTo(before, true)
	case ctx.Err() != nil:
		return // the watch ends; its end prints what is held
	default:
		if timedOut {
			fmt.Fprintf(os.Stderr, "arc-dm: the read of the outbox took more than %v; the held messages show with no quote\n", readTimeout)
		} else {
			fmt.Fprintln(os.Stderr, "arc-dm: the read of the outbox failed:", terminal.EscapeLine(err.Error()))
		}
		w.readOK = false
		w.flushTo(len(w.queue), false)
	}
	if w.waiting == 0 {
		w.flushTo(len(w.queue), false)
		return
	}
	w.schedule()
}

// flushTo prints the held messages that came before place, in order, and
// holds the rest again. After a read of the outbox (read true), a parent
// that is still not here is missed, and no later reply waits for it.
//
// Without a read, a reply that waits shows with no quote line: not "(not
// on this device)", because its parent can be on this device, and no
// parent is missed.
func (w *watcher) flushTo(place int, read bool) {
	held, rest := w.queue[:place], w.queue[place:]
	w.queue, w.live, w.waiting = nil, 0, 0
	clear(w.bySender)
	for _, m := range held {
		if m == nil {
			continue
		}
		waits := w.waits(*m)
		if read && waits {
			w.miss(m.ReplyTo)
		}
		w.print(*m, w.messages, read || !waits)
	}
	for _, m := range rest {
		if m != nil {
			w.hold(*m)
		}
	}
}

// miss remembers an id that a read of the outbox did not find.
func (w *watcher) miss(id string) {
	if len(w.missed) >= maxMissed {
		clear(w.missed)
	}
	w.missed[id] = true
}

// close prints the messages that still wait, at the end of the watch. If
// one waits for a parent, it reads the outbox once more, for at most
// closeRead, or until a second interrupt. It drops no message: without a
// read, it prints them as they are.
func (w *watcher) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	w.stop()
	for w.reading {
		w.cond().Wait()
	}
	read := false
	if w.waiting > 0 {
		parent := w.closeCtx
		if parent == nil {
			parent = context.Background()
		}
		ctx, cancel := context.WithTimeout(parent, closeRead)
		outbox, err := w.outbox(ctx)
		cancel()
		if read = err == nil; read {
			for _, sent := range sentMessages(outbox) {
				w.messages.add(sent.entry, conversation{Peers: peersOf(w.me, sent.members)})
			}
		}
	}
	w.flushTo(len(w.queue), read)
	w.cond().Broadcast()
}
