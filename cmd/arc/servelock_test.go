package main

import (
	"strings"
	"testing"
	"time"
)

// A serve that finds another serve of the capability in the home waits,
// says so once, and gives up with an error that names it and what to do.
// The lock is per capability: another capability takes its own (#447).
func TestASecondServeWaitsThenGivesUp(t *testing.T) {
	defer func(old time.Duration) { serveWait = old }(serveWait)
	serveWait = 500 * time.Millisecond
	dir := t.TempDir()
	release, err := holdServe(&strings.Builder{}, dir, "exec")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	var said strings.Builder
	if _, err := holdServe(&said, dir, "exec"); err == nil || !strings.Contains(err.Error(), "another arc serve of exec in this home runs") || !strings.Contains(err.Error(), "stop it first") {
		t.Errorf("a second serve gave %v, want it to give up with a clear error", err)
	}
	if strings.Count(said.String(), "waiting for another arc serve of exec in this home") != 1 {
		t.Errorf("a second serve said %q, want one line that it waits", said.String())
	}
	other, err := holdServe(&strings.Builder{}, dir, "files")
	if err != nil {
		t.Fatalf("a serve of another capability did not take its own lock: %v", err)
	}
	other()
}
