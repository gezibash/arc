package store_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gezibash/arc/delivery/store"
	"go.etcd.io/bbolt"
)

// boltLease opens one bolt file, as a second process would: bolt locks the
// file for each open, also within one process.
func boltLease(path string) *store.Lease[*bbolt.DB] {
	return store.NewLease(path, func() (*bbolt.DB, error) {
		return bbolt.Open(path, 0o600, &bbolt.Options{Timeout: 50 * time.Millisecond})
	}, func(db *bbolt.DB) { db.Close() })
}

func TestACallInsideACallDoesNotWaitForItself(t *testing.T) {
	// No deferred Close: if the call waits for itself, Close waits too.
	l := boltLease(filepath.Join(t.TempDir(), "x.db"))
	done := make(chan error, 1)
	go func() {
		done <- l.Do(func(*bbolt.DB) error {
			return l.Do(func(*bbolt.DB) error { return nil })
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
		l.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("a call inside a call waited for itself")
	}
}

// Two leases on one file stand for two commands of one identity. The second
// waits while the first is in a call, and gets the file after the first
// leaves it idle.
func TestASecondHolderGetsTheFileWhenTheFirstIsIdle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	first, second := boltLease(path), boltLease(path)
	defer first.Close()
	defer second.Close()

	inside, leave := make(chan struct{}), make(chan struct{})
	go first.Do(func(*bbolt.DB) error {
		close(inside)
		<-leave
		return nil
	})
	<-inside

	got := make(chan time.Time, 1)
	go func() {
		if err := second.Do(func(*bbolt.DB) error { return nil }); err != nil {
			t.Error(err)
		}
		got <- time.Now()
	}()
	select {
	case <-got:
		t.Fatal("the second holder got the file while the first was in a call")
	case <-time.After(300 * time.Millisecond):
	}

	left := time.Now()
	close(leave)
	select {
	case at := <-got:
		if waited := at.Sub(left); waited < store.Idle/2 {
			t.Errorf("the second holder got the file %s after the first left, before its idle time", waited)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second holder never got the file")
	}

	// The first holder opens the file again when the second leaves it.
	if err := first.Do(func(*bbolt.DB) error { return nil }); err != nil {
		t.Fatalf("the first holder could not open the file again: %v", err)
	}
}

func TestAClosedLeaseRefusesCalls(t *testing.T) {
	l := boltLease(filepath.Join(t.TempDir(), "x.db"))
	if err := l.Do(func(*bbolt.DB) error { return nil }); err != nil {
		t.Fatal(err)
	}
	l.Close()
	if err := l.Do(func(*bbolt.DB) error { return nil }); err == nil {
		t.Error("a closed lease ran a call")
	}
}
