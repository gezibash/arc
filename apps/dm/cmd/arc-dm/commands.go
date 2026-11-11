package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/terminal"
)

// list prints the conversations, newest first.
func list(ctx context.Context, args []string) error {
	flags, s := newFlags("ls")
	args, err := parse(flags, args, 0)
	if err != nil {
		return err
	}
	if len(args) != 0 {
		return errors.New("usage: arc-dm ls [flags]")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	read, err := loadRead(stateDir(s.state))
	if err != nil {
		return err
	}
	if len(all) == 0 {
		fmt.Println("no conversations: run arc sync, or keep arc-dm watch running")
		return nil
	}
	for _, c := range all {
		last := c.last()
		status := ""
		if n := c.unread(read); n > 0 {
			status = fmt.Sprintf("  %d unread", n)
		}
		who := ""
		switch {
		case last.Mine:
			who = "you: "
		case !c.direct():
			who = last.From + ": "
		}
		fmt.Printf("%s  %s%s\n  %s%s\n", stamp(last.At), c.Name, status, who, terminal.Escape(preview(last.Text, 72))) //arc:raw-ok c.Name holds petnames; status is arc-dm's own
	}
	return nil
}

// open prints one conversation, and marks its messages read.
func open(ctx context.Context, args []string) error {
	flags, s := newFlags("open")
	args, err := parse(flags, args, 1)
	if err != nil {
		return err
	}
	if len(args) != 1 {
		return errors.New("usage: arc-dm open [flags] <peer>")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	c, err := find(all, args[0])
	if err != nil {
		return err
	}
	c = sharedGroup(all, c)
	if len(c.Entries) == 0 {
		fmt.Println(noMessages(all, c))
		return nil
	}
	dir := stateDir(s.state)
	read, err := loadRead(dir)
	if err != nil {
		return err
	}
	messages := index(all)
	for _, e := range c.Entries {
		who, status := cmp.Or(e.From, c.Name), ""
		if e.Mine {
			who, status = "you", "  "+e.State
		} else if !read[e.ID] {
			status = "  new"
		}
		fmt.Printf("%s  %s  %s%s\n", stamp(e.At), messages.short(e.ID), who, status) //arc:raw-ok status is a delivery state of arc, or new; the id is hex
		if q := messages.quote(e, c); q != "" {
			fmt.Println(q)
		}
		fmt.Println(indent(terminal.Escape(e.Text)))
	}
	if err := saveShown(dir, messages.shown); err != nil {
		return err
	}
	return markRead(dir, c)
}

// send sends a message to a peer.
func send(ctx context.Context, args []string) error {
	flags, s := newFlags("send")
	replyTo := flags.String("reply", "", "reply to the message with this id, or the start of it")
	args, literal, err := parseText(flags, args, 1, "reply")
	if err != nil {
		return err
	}
	if len(args) < 1 {
		return errors.New("usage: arc-dm send [flags] <peer> [text...]")
	}
	if err := refuseHelp(args[1:], literal, "send <peer>"); err != nil {
		return err
	}
	if given(flags, "reply") {
		// An empty -reply= must not send a plain message.
		if err := checkID(*replyTo); err != nil {
			return err
		}
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	c, err := find(all, args[0])
	if err != nil {
		return err
	}
	if !given(flags, "reply") {
		text, err := messageText(ctx, args[1:])
		if err != nil {
			return err
		}
		if _, err := s.arc.Send(ctx, c.Peers, text); err != nil {
			return err
		}
		fmt.Printf("queued for %s\n", cmp.Or(c.Name, c.key())) //arc:raw-ok c.Name holds petnames, or a key
		return nil
	}
	// The id is checked before the text, so a wrong id does not wait for
	// standard input.
	messages := index(all)
	if messages.avoid, err = loadShown(stateDir(s.state)); err != nil {
		return err
	}
	parent, err := messages.lookup(*replyTo)
	if err != nil {
		return err
	}
	if parent.conv.key() != c.key() {
		return fmt.Errorf("%s is in another conversation: arc-dm reply %s sends to its members", messages.short(parent.ID), messages.short(parent.ID))
	}
	text, err := messageText(ctx, args[1:])
	if err != nil {
		return err
	}
	return sendReply(ctx, s.arc, messages, parent, text)
}

// reply sends a reply to a message, to each member of its conversation.
func reply(ctx context.Context, args []string) error {
	flags, s := newFlags("reply")
	args, literal, err := parseText(flags, args, 1)
	if err != nil {
		return err
	}
	if len(args) < 1 {
		return errors.New("usage: arc-dm reply [flags] <id> [text...]")
	}
	if err := refuseHelp(args[1:], literal, "reply <id>"); err != nil {
		return err
	}
	if err := checkID(args[0]); err != nil {
		return err
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	messages := index(all)
	if messages.avoid, err = loadShown(stateDir(s.state)); err != nil {
		return err
	}
	parent, err := messages.lookup(args[0])
	if err != nil {
		return err
	}
	text, err := messageText(ctx, args[1:])
	if err != nil {
		return err
	}
	return sendReply(ctx, s.arc, messages, parent, text)
}

// sendReply sends text to the members of the parent, and names them: a
// mistyped id could answer in another conversation.
func sendReply(ctx context.Context, arc arcprogram.Arc, messages *store, parent held, text string) error {
	if _, err := arc.SendReply(ctx, parent.conv.Peers, parent.ID, text); err != nil {
		return err
	}
	fmt.Printf("replied to %s in %s\n", messages.short(parent.ID), parent.conv.Name) //arc:raw-ok the id is hex; the name holds petnames
	return nil
}

// messageText is the words of a command, or standard input when there are
// none.
func messageText(ctx context.Context, words []string) (string, error) {
	text := strings.Join(words, " ")
	if len(words) == 0 {
		body, err := readAll(ctx, os.Stdin)
		if err != nil {
			return "", err
		}
		text = strings.TrimRight(string(body), "\n")
	}
	if terminal.Blank(text) {
		return "", errors.New("a message holds at least one character that a person can see")
	}
	return text, nil
}

// readAll reads r to its end, or returns at the end of ctx: a read of
// standard input cannot be stopped, and a signal must still stop arc-dm.
func readAll(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		body []byte
		err  error
	}
	done := make(chan result, 1)
	go func() {
		body, err := io.ReadAll(r)
		done <- result{body, err}
	}()
	select {
	case got := <-done:
		return got.body, got.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// read marks the messages of one conversation, or of all, read.
func read(ctx context.Context, args []string) error {
	flags, s := newFlags("read")
	args, err := parse(flags, args, 1)
	if err != nil {
		return err
	}
	if len(args) > 1 {
		return errors.New("usage: arc-dm read [flags] [peer]")
	}
	all, err := conversations(ctx, s.arc)
	if err != nil {
		return err
	}
	if len(args) == 1 {
		c, err := find(all, args[0])
		if err != nil {
			return err
		}
		all = []conversation{sharedGroup(all, c)}
	}
	return markRead(stateDir(s.state), all...)
}
