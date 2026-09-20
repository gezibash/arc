package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gezibash/arc/go/provider"
)

// The search index is the qmd tool, which reads the repository. A write
// starts a reindex after a pause, so no write waits for one.
const reindexPause = 2 * time.Second

// The push to a remote also waits, so a run of writes pushes once.
const pushPause = 30 * time.Second

type index struct {
	root string
	log  *os.File

	mu    sync.Mutex
	timer *time.Timer
}

func newIndex(root string) *index {
	held := &index{root: root, log: os.Stderr}
	held.setup()
	return held
}

// available says whether the search tool is on this machine.
func (i *index) available() bool {
	_, err := exec.LookPath("qmd")
	return err == nil
}

// search asks the index. A deep search reads more than the titles.
func (i *index) search(query string, deep bool) (string, error) {
	command := "search"
	if deep {
		command = "query"
	}

	out, err := i.qmd(command, query, "-n", "10", "--full-path")
	if err != nil {
		return "", err
	}
	return out, nil
}

// touch starts a reindex after the pause, and puts off one that waits.
func (i *index) touch() {
	if !i.available() {
		return
	}

	i.mu.Lock()
	defer i.mu.Unlock()

	if i.timer != nil {
		i.timer.Stop()
	}
	i.timer = time.AfterFunc(reindexPause, func() {
		if _, err := i.qmd("update"); err != nil {
			fmt.Fprintln(i.log, "journal:", err)
		}
	})
}

// setup makes the index and names the notebooks, once.
func (i *index) setup() {
	if !i.available() {
		return
	}

	if info, err := os.Stat(filepath.Join(i.root, ".qmd")); err != nil || !info.IsDir() {
		i.qmd("init")
	}

	out, err := i.qmd("collection", "list")
	if err != nil {
		return
	}
	if !strings.Contains(out, "journal") {
		i.qmd("collection", "add", filepath.Join(i.root, "repo/projects"), "--name", "journal")
	}
}

func (i *index) qmd(args ...string) (string, error) {
	if !i.available() {
		return "", provider.Error("search_unavailable")
	}

	command := exec.Command("qmd", args...)
	command.Dir = i.root
	command.Env = append(os.Environ(), "PWD="+i.root)

	out, err := command.CombinedOutput()
	if err != nil {
		return "", provider.Error("qmd failed: " + strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// pusher sends the repository to a remote after a pause. Without a remote it
// does nothing.
type pusher struct {
	store  *store
	remote string
	log    *os.File

	mu    sync.Mutex
	timer *time.Timer
}

func (p *pusher) touch() {
	if p == nil || p.remote == "" {
		return
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(pushPause, func() {
		if err := p.store.push(p.remote); err != nil {
			fmt.Fprintln(p.log, "journal: push failed:", err)
		}
	})
}
