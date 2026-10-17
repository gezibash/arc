package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"
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
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// messageLine is one received message in JSON. inbox --json and watch --json
// print it, one object on each line. Programs outside this repository read
// it, so a change to it is a breaking change. docs/clients/SPEC.md defines
// it.
type messageLine struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Name string `json:"name"`
	At   string `json:"at"`
	Text string `json:"text"`
	// Members are the sender, then each member that a p tag names.
	Members []string `json:"members"`
}

// sentLine is one sent message in JSON. outbox --json prints it, one object
// on each line, and one line for each member of a group message. Programs
// outside this repository read it, so a change to it is a breaking change.
type sentLine struct {
	ID      string   `json:"id"`
	To      string   `json:"to"`
	Name    string   `json:"name"`
	At      string   `json:"at"`
	Text    string   `json:"text"`
	State   string   `json:"state"`
	Members []string `json:"members"`
}

// queuedLine is the message that send --json put in the outbox. To is the
// first recipient.
type queuedLine struct {
	ID      string   `json:"id"`
	To      string   `json:"to"`
	State   string   `json:"state"`
	Members []string `json:"members"`
}

// recipients reads the public keys of a send, separated by commas.
func recipients(text string) ([]nostr.PubKey, error) {
	var out []nostr.PubKey
	for part := range strings.SplitSeq(text, ",") {
		pk, err := nostr.PubKeyFromHex(strings.TrimSpace(part))
		if err != nil {
			return nil, errors.New("a recipient is 64 characters of hex, as arc whoami prints it; separate the recipients of a group with commas")
		}
		out = append(out, pk)
	}
	return out, nil
}

func hexKeys(pks []nostr.PubKey) []string {
	out := make([]string, len(pks))
	for i, pk := range pks {
		out[i] = pk.Hex()
	}
	return out
}

// petnames names some keys, separated by commas.
func petnames(pks []nostr.PubKey) string {
	names := make([]string, len(pks))
	for i, pk := range pks {
		names[i] = keys.Name(pk[:])
	}
	return strings.Join(names, ", ")
}

// printOutbox prints each sent message once. A group message says to how
// many of its members it was delivered.
func printOutbox(w io.Writer, out []mail.Outgoing, now time.Time) {
	var order []string
	byRumor := map[string][]mail.Outgoing{}
	for _, o := range out {
		if byRumor[o.Rumor] == nil {
			order = append(order, o.Rumor)
		}
		byRumor[o.Rumor] = append(byRumor[o.Rumor], o)
	}
	for _, rumor := range order {
		entries := byRumor[rumor]
		var to []nostr.PubKey
		delivered, expired, attempts := 0, 0, 0
		for _, o := range entries {
			pk, _ := nostr.PubKeyFromHex(o.To)
			to = append(to, pk)
			switch o.State(now) {
			case "delivered":
				delivered++
			case "expired":
				expired++
			}
			attempts = max(attempts, o.Attempts)
		}
		state := entries[0].State(now)
		if len(entries) > 1 && delivered < len(entries) && (delivered > 0 || expired > 0) {
			state = fmt.Sprintf("delivered to %d of %d", delivered, len(entries))
			if expired > 0 {
				state += fmt.Sprintf(", %d expired", expired)
			}
		}
		times := "times"
		if attempts == 1 {
			times = "time"
		}
		fmt.Fprintf(w, "%s  to %s  %s, sent %d %s\n  %s\n",
			entries[0].Created.Local().Format("2006-01-02 15:04"), petnames(to), state, attempts, times, shown(entries[0].Text))
	}
}

// printSent prints the sent messages of the outbox as JSON lines. It leaves
// out requests, and private events of the kinds of apps: the outbox holds
// them too.
func printSent(command *cobra.Command, m *mail.Mail, out []mail.Outgoing) error {
	rumors, err := m.Rumors(command.Context(), []nostr.Kind{mail.MessageKind})
	if err != nil {
		return err
	}
	members := map[string][]string{}
	for _, rumor := range rumors {
		members[rumor.ID.Hex()] = hexKeys(mail.Members(rumor))
	}
	encoder := json.NewEncoder(command.OutOrStdout())
	encoder.SetEscapeHTML(false)
	now := time.Now()
	for _, o := range out {
		if o.Kind != "message" || members[o.Rumor] == nil {
			continue
		}
		to, err := nostr.PubKeyFromHex(o.To)
		if err != nil {
			return err
		}
		if err := encoder.Encode(sentLine{
			ID: o.Rumor, To: o.To, Name: keys.Name(to[:]),
			At: o.Created.UTC().Format(time.RFC3339), Text: o.Text, State: o.State(now),
			Members: members[o.Rumor],
		}); err != nil {
			return err
		}
	}
	return nil
}

// shown is the text of a message as a terminal can show it under its
// header: escaped, and with each line indented, so no line of the text
// looks like the header of another message.
func shown(text string) string {
	return strings.ReplaceAll(terminal.Escape(text), "\n", "\n  ")
}

// messagePrinter writes received messages as inbox prints them, or as JSON.
type messagePrinter struct {
	out     io.Writer
	me      nostr.PubKey
	encoder *json.Encoder
}

func newMessagePrinter(out io.Writer, me nostr.PubKey, asJSON bool) messagePrinter {
	p := messagePrinter{out: out, me: me}
	if asJSON {
		p.encoder = json.NewEncoder(out)
		p.encoder.SetEscapeHTML(false)
	}
	return p
}

func (p messagePrinter) print(m mail.Message) error {
	if p.encoder == nil {
		group := ""
		if len(m.Members) > 2 {
			others := slices.DeleteFunc(slices.Clone(m.Members[1:]), func(pk nostr.PubKey) bool { return pk == p.me })
			group = "  to you, " + petnames(others)
		}
		_, err := fmt.Fprintf(p.out, "%s  %s%s\n  %s\n", m.At.Local().Format("2006-01-02 15:04"), keys.Name(m.From[:]), group, shown(m.Text))
		return err
	}
	return p.encoder.Encode(messageLine{
		ID: m.ID, From: m.From.Hex(), Name: keys.Name(m.From[:]),
		At: m.At.UTC().Format(time.RFC3339), Text: m.Text, Members: hexKeys(m.Members),
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

	printer := newMessagePrinter(command.OutOrStdout(), sess.Signer.PublicKey(), asJSON)
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
	after, err := sinceFilter(ctx, sess, since, msgs)
	if err != nil {
		return err
	}
	// showNew prints the messages that it did not print before, in the
	// order of receipt. An adapter that gives the last id to --since after
	// a restart then loses no message.
	seen := map[string]bool{}
	showNew := func(msgs []mail.Message, include func(mail.Message) bool) error {
		var fresh []mail.Message
		for _, m := range msgs {
			if !seen[m.ID] {
				seen[m.ID] = true
				if include(m) {
					fresh = append(fresh, m)
				}
			}
		}
		if len(fresh) > 1 {
			order, err := sess.ReceiptOrder(ctx)
			if err != nil {
				return err
			}
			// A message with no receipt comes first, in the order of the inbox.
			slices.SortStableFunc(fresh, func(a, b mail.Message) int { return cmp.Compare(order[a.ID], order[b.ID]) })
		}
		for _, m := range fresh {
			if err := show(m); err != nil {
				return err
			}
		}
		return nil
	}
	if err := showNew(msgs, after); err != nil {
		return err
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
		if err := showNew(msgs, func(mail.Message) bool { return true }); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

// sinceFilter says which stored messages watch prints before it watches.
// With no point, it prints none. A time includes the messages at that time,
// by the clock of each sender. A message id selects the messages that this
// machine stored after that message, and each message that it stored before
// it kept receipts.
func sinceFilter(ctx context.Context, sess *citizen.Session, since string, msgs []mail.Message) (func(mail.Message) bool, error) {
	if since == "" {
		return func(mail.Message) bool { return false }, nil
	}
	if at, err := time.Parse(time.RFC3339, since); err == nil {
		return func(m mail.Message) bool { return !m.At.Before(at) }, nil
	}
	if !slices.ContainsFunc(msgs, func(m mail.Message) bool { return m.ID == since }) {
		return nil, fmt.Errorf("--since %q is not an RFC 3339 time or the id of a message in the inbox", since)
	}
	order, err := sess.ReceiptOrder(ctx)
	if err != nil {
		return nil, err
	}
	mark := order[since]
	return func(m mail.Message) bool {
		position, ok := order[m.ID]
		return m.ID != since && (!ok || position > mark)
	}, nil
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
