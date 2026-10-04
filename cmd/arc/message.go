package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
	"github.com/spf13/cobra"
)

// messageLine is one received message in JSON. inbox --json and watch --json
// print it, one object on each line. Programs outside this repository read
// it, so a change to it is a breaking change.
type messageLine struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Name string `json:"name"`
	At   string `json:"at"`
	Text string `json:"text"`
}

// messagePrinter writes received messages as inbox prints them, or as JSON.
type messagePrinter struct {
	out     io.Writer
	encoder *json.Encoder
}

func newMessagePrinter(out io.Writer, asJSON bool) messagePrinter {
	p := messagePrinter{out: out}
	if asJSON {
		p.encoder = json.NewEncoder(out)
		p.encoder.SetEscapeHTML(false)
	}
	return p
}

func (p messagePrinter) print(m mail.Message) error {
	if p.encoder == nil {
		_, err := fmt.Fprintf(p.out, "%s  %s\n  %s\n", m.At.Local().Format("2006-01-02 15:04"), keys.Name(m.From[:]), m.Text)
		return err
	}
	return p.encoder.Encode(messageLine{
		ID: m.ID, From: m.From.Hex(), Name: keys.Name(m.From[:]),
		At: m.At.UTC().Format(time.RFC3339), Text: m.Text,
	})
}

func messageWatchCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "watch",
		Short: "Print each message as it arrives, until a signal stops it",
		Long: "watch holds a watch on each relay, and prints each new message on\n" +
			"standard output. It never marks a message read. A message can come\n" +
			"twice, for example after a restart: remove duplicates by id.\n" +
			"With --since, watch first prints the stored messages from that point.",
		Args: cobra.NoArgs,
		RunE: watchMessages,
	}
	command.Flags().Bool("json", false, "print one JSON object for each message")
	command.Flags().String("since", "", "first print the stored messages at or after this RFC 3339 time, or after this message id")
	command.Flags().StringArray("from", nil, "print only the messages from this public key (repeatable)")
	return command
}

func watchMessages(command *cobra.Command, _ []string) error {
	asJSON, _ := command.Flags().GetBool("json")
	since, _ := command.Flags().GetString("since")
	texts, _ := command.Flags().GetStringArray("from")
	senders := map[nostr.PubKey]bool{}
	for _, text := range texts {
		pk, err := nostr.PubKeyFromHex(text)
		if err != nil {
			return fmt.Errorf("--from %q is not a public key", text)
		}
		senders[pk] = true
	}

	ctx, stop := signal.NotifyContext(command.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	var relays []transport.Live
	for _, t := range sess.Relays {
		if live, ok := t.(transport.Live); ok {
			relays = append(relays, live)
		}
	}
	if len(relays) == 0 {
		return errors.New("arc message watch needs a relay: run arc relay add <url>")
	}

	// The mail of a watch saves each seal that it opens. After each save,
	// the inbox can hold a new message.
	saved := make(chan struct{}, 1)
	sess.Node.Store = sealNotice{EventStore: sess.Node.Store, saved: saved}

	printer := newMessagePrinter(os.Stdout, asJSON)
	show := func(m mail.Message) error {
		if len(senders) > 0 && !senders[m.From] {
			return nil
		}
		return printer.print(m)
	}
	msgs, err := sess.Mail.Inbox(ctx)
	if err != nil {
		return err
	}
	after, err := sinceFilter(since, msgs)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, m := range msgs {
		seen[m.ID] = true
		if after(m) {
			if err := show(m); err != nil {
				return err
			}
		}
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	// The store closes after the watches end.
	var watches sync.WaitGroup
	defer watches.Wait()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, r := range relays {
		watches.Go(func() {
			keepMail(ctx, sess.Mail, r, func() { log.Info("watching", "relay", r.Name()) }, log)
		})
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-saved:
		}
		msgs, err := sess.Mail.Inbox(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			return err
		}
		for _, m := range msgs {
			if seen[m.ID] {
				continue
			}
			seen[m.ID] = true
			if err := show(m); err != nil {
				return err
			}
		}
	}
}

// sinceFilter says which stored messages watch prints before it watches.
// With no point, it prints none. A time includes the messages at that time.
// A message id includes the other messages at the time of that message,
// because the inbox does not order messages of the same second.
func sinceFilter(since string, msgs []mail.Message) (func(mail.Message) bool, error) {
	if since == "" {
		return func(mail.Message) bool { return false }, nil
	}
	if at, err := time.Parse(time.RFC3339, since); err == nil {
		return func(m mail.Message) bool { return !m.At.Before(at) }, nil
	}
	for _, mark := range msgs {
		if mark.ID == since {
			return func(m mail.Message) bool { return m.ID != mark.ID && !m.At.Before(mark.At) }, nil
		}
	}
	return nil, fmt.Errorf("--since %q is not an RFC 3339 time or the id of a message in the inbox", since)
}

// sealNotice tells a watch that the store saved a seal. A seal that the store
// held already also counts: another arc command can save a message first.
type sealNotice struct {
	node.EventStore
	saved chan<- struct{}
}

func (s sealNotice) Save(event nostr.Event) (store.Result, error) {
	result, err := s.EventStore.Save(event)
	if event.Kind == private.SealKind {
		select {
		case s.saved <- struct{}{}:
		default:
		}
	}
	return result, err
}
