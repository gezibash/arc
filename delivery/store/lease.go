package store

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"go.etcd.io/bbolt"
)

// A bolt file takes one process at a time. A command of arc can run for
// hours, as tail and serve do, so a store does not keep its file open. It
// opens the file when a call needs it, and closes it when no call used it for
// Idle. Another process then gets the file.
//
// To open a file costs about 20 ms, because bolt writes to it. A lease
// therefore keeps the file open through a burst of calls, such as a sync.

// Idle is how long a lease keeps a file open after its last call.
const Idle = 200 * time.Millisecond

// Wait is how long a lease waits for another process to close a file.
const Wait = 30 * time.Second

// Lease opens a file on the first call, and closes it after Idle.
type Lease[T any] struct {
	name  string
	open  func() (T, error)
	close func(T)

	mu    sync.Mutex
	value T
	held  bool
	users int
	timer *time.Timer
	done  bool
}

// NewLease makes a lease. The name says which file it is, in errors.
func NewLease[T any](name string, open func() (T, error), close func(T)) *Lease[T] {
	return &Lease[T]{name: name, open: open, close: close}
}

// Do runs fn with the open file. Calls can nest, and can run at once: the
// file closes only when no call uses it.
func (l *Lease[T]) Do(fn func(T) error) error {
	l.mu.Lock()
	if l.done {
		l.mu.Unlock()
		return fmt.Errorf("store: %s is closed", l.name)
	}
	if !l.held {
		value, err := l.wait()
		if err != nil {
			l.mu.Unlock()
			return err
		}
		l.value, l.held = value, true
	}
	if l.timer != nil {
		l.timer.Stop()
	}
	l.users++
	value := l.value
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.users--
		if l.users == 0 && !l.done {
			l.timer = time.AfterFunc(Idle, l.release)
		}
	}()
	return fn(value)
}

// wait opens the file. While another process holds it, bolt refuses after
// its own timeout, and wait tries again until Wait passes.
func (l *Lease[T]) wait() (T, error) {
	deadline := time.Now().Add(Wait)
	for {
		value, err := l.open()
		if err == nil || !errors.Is(err, bbolt.ErrTimeout) {
			return value, err
		}
		if time.Now().After(deadline) {
			return value, fmt.Errorf("store: another process has held %s for %s: %w", l.name, Wait, err)
		}
	}
}

// release closes the file when no call uses it.
func (l *Lease[T]) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.users == 0 && l.held {
		l.close(l.value)
		l.held = false
	}
}

// Close closes the file, and refuses later calls.
func (l *Lease[T]) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.timer != nil {
		l.timer.Stop()
	}
	if l.held {
		l.close(l.value)
		l.held = false
	}
	l.done = true
}
