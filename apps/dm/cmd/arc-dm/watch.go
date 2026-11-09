package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/terminal"
)

// watch prints each message as it arrives, and posts a notification. It
// marks no message read.
func watch(ctx context.Context, args []string) error {
	flags, s := newFlags("watch")
	notify := flags.String("notify", "", "a program to run for each message, as <program> <sender> <text>; none for no notification (default: the notifications of this system)")
	args, err := parse(flags, args, 0)
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return errors.New("usage: arc-dm watch [flags]")
	}
	notifier := notifierOf(*notify)
	me, err := s.arc.PublicKey(ctx)
	if err != nil {
		return err
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	w := &watcher{
		outbox:   s.arc.Outbox,
		me:       me,
		messages: index(all),
		missed:   map[string]bool{},
		bySender: map[string][]int{},
		lastEnd:  time.Now(), // conversations read the outbox
		readOK:   true,
	}
	w.ctx = ctx
	if second, ok := ctx.Value(secondSignal{}).(context.Context); ok {
		w.closeCtx = second
	}
	w.print = func(m arcprogram.Message, messages *store, quote bool) {
		group := ""
		if len(m.Members) > 2 {
			group = fmt.Sprintf("  in a group of %d", len(m.Members))
		}
		members := m.Members
		if members == nil {
			// An arc older than group messages prints no members.
			members = []string{m.From}
		}
		c := conversation{Peers: peersOf(me, members)}
		e := entry{ID: m.ID, At: m.At, Text: m.Text, From: m.Name, ReplyTo: m.ReplyTo}
		messages.add(e, c)
		fmt.Printf("%s  %s  %s%s\n", stamp(m.At), messages.short(m.ID), m.Name, group) //arc:raw-ok m.Name is a petname; the id is hex
		// With no quote line, no id is shown as missing.
		if quote {
			if q := messages.quote(e, c); q != "" {
				fmt.Println(q)
			}
		}
		fmt.Println(indent(terminal.Escape(m.Text)))
		if len(notifier) == 0 {
			return
		}
		body := terminal.Escape(preview(m.Text, 200))
		if filepath.Base(notifier[0]) == "notify-send" {
			// Many notification servers read the body as markup.
			body = html.EscapeString(body)
		}
		command := exec.CommandContext(ctx, notifier[0], append(notifier[1:], m.Name, body)...)
		if out, err := command.CombinedOutput(); err != nil && ctx.Err() == nil {
			// A failed notification does not stop the watch.
			fmt.Fprintf(os.Stderr, "arc-dm: the notification failed: %s\n", terminal.EscapeLine(err.Error()+" "+strings.TrimSpace(string(out))))
		}
	}
	// The ids that the watch shows as missing go to the state at most once
	// a second, and at its end, so a reply in another terminal refuses them.
	// The watch also saves after each timer read that showed an id.
	var (
		saving   sync.Mutex
		lastSave time.Time
	)
	save := func(force bool) {
		saving.Lock()
		defer saving.Unlock()
		if !force && time.Since(lastSave) < time.Second {
			return
		}
		w.mu.Lock()
		ids := w.messages.shown
		w.messages.shown, w.messages.isShown = nil, nil
		w.mu.Unlock()
		lastSave = time.Now()
		if err := saveShown(stateDir(s.state), ids); err != nil {
			fmt.Fprintln(os.Stderr, "arc-dm:", terminal.EscapeLine(err.Error()))
		}
	}
	w.mu.Lock()
	w.saveShown = func() { save(true) } // reads come at most each second
	w.mu.Unlock()
	err = s.arc.Watch(ctx, arcprogram.WatchOptions{Notes: os.Stderr}, func(m arcprogram.Message) error {
		w.take(m)
		save(false)
		return nil
	})
	w.close()
	save(true)
	return err
}

// notifierOf is the command that posts a notification, before its two
// arguments: the sender and the text.
func notifierOf(setting string) []string {
	switch setting {
	case "none":
		return nil
	case "":
	default:
		return []string{setting}
	}
	switch runtime.GOOS {
	case "darwin":
		return []string{"osascript", "-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)", "-e", "end run"}
	case "linux":
		if _, err := exec.LookPath("notify-send"); err == nil {
			return []string{"notify-send", "--"}
		}
	}
	return nil
}
