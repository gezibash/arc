package main

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// leaseTimeout bounds one lease command. A command that hangs must not hold
// the provider.
const leaseTimeout = 10 * time.Second

// lease keeps the machine awake while one or more commands run.
//
// The first command runs the hold command. A goroutine runs it again after
// each interval while commands run. The last command runs the release
// command. Every lease command runs under one lock, so a late refresh never
// follows a release.
type lease struct {
	config *leaseConfig
	log    io.Writer
	run    func(argv []string)

	mu      sync.Mutex
	changed *sync.Cond
	active  int
	started bool
}

func newLease(config *leaseConfig, log io.Writer) *lease {
	held := &lease{config: config, log: log}
	held.changed = sync.NewCond(&held.mu)
	held.run = held.runCommand
	return held
}

// hold takes one more claim on the machine.
func (l *lease) hold() {
	if l == nil || l.config == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.active++
	if l.active == 1 {
		l.run(l.config.Hold)
	}
	if !l.started {
		l.started = true
		go l.refresh()
	}
	l.changed.Broadcast()
}

// release gives back one claim. The last claim releases the machine.
func (l *lease) release() {
	if l == nil || l.config == nil {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	l.active--
	if l.active == 0 {
		l.run(l.config.Release)
	}
	l.changed.Broadcast()
}

// refresh runs the hold command again while work is running.
func (l *lease) refresh() {
	interval := time.Duration(l.config.IntervalMS) * time.Millisecond

	for {
		l.mu.Lock()
		for l.active == 0 {
			l.changed.Wait()
		}
		l.mu.Unlock()

		time.Sleep(interval)

		l.mu.Lock()
		if l.active > 0 {
			l.run(l.config.Hold)
		}
		l.mu.Unlock()
	}
}

// runCommand runs one lease command. Standard output carries the ARC
// protocol, so the output of a lease command never reaches it.
func (l *lease) runCommand(argv []string) {
	ctx, cancel := context.WithTimeout(context.Background(), leaseTimeout)
	defer cancel()

	process := exec.CommandContext(ctx, argv[0], argv[1:]...)
	output, err := process.CombinedOutput()

	if err != nil {
		fmt.Fprintf(l.log, "the lease command failed: %v: %s\n", err, trimOutput(output))
	}
}

func trimOutput(output []byte) string {
	if len(output) > 500 {
		output = output[:500]
	}
	return safeText(output)
}
