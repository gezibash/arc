package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	// ReplyTo is the id of the parent of a reply.
	ReplyTo string `json:"reply_to,omitempty"`
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
	ReplyTo string   `json:"reply_to,omitempty"`
}

// queuedLine is the message that send --json put in the outbox. To is the
// first recipient.
type queuedLine struct {
	ID      string   `json:"id"`
	To      string   `json:"to"`
	State   string   `json:"state"`
	Members []string `json:"members"`
	ReplyTo string   `json:"reply_to,omitempty"`
}

// sendMessage sends a message, or with --reply a reply to a message of
// this machine.
func sendMessage(command *cobra.Command, sess *citizen.Session, to []nostr.PubKey, text string) ([]mail.Outgoing, error) {
	if tags := testTags(command, to); tags != nil {
		return sess.Mail.SendRumor(command.Context(), to, mail.MessageKind, text, tags)
	}
	parent, err := replyParent(command)
	if err != nil {
		return nil, err
	}
	if parent == (nostr.ID{}) {
		return sess.Mail.Send(command.Context(), to, text)
	}
	return sess.Mail.SendReply(command.Context(), to, parent, text)
}

// replyParent is the id of --reply, or a zero id for no reply.
func replyParent(command *cobra.Command) (nostr.ID, error) {
	reply, _ := command.Flags().GetString("reply")
	if reply == "" {
		return nostr.ID{}, nil
	}
	parent, err := nostr.IDFromHex(reply)
	if err != nil || strings.ToLower(reply) != reply {
		return nostr.ID{}, errors.New("--reply takes the id of a message: 64 lowercase hex digits, as arc message inbox --json prints it")
	}
	return parent, nil
}

// recipients reads the public keys of a send, separated by commas.
func recipients(text string) ([]nostr.PubKey, error) {
	var out []nostr.PubKey
	for part := range strings.SplitSeq(text, ",") {
		pk, err := publicKey(strings.TrimSpace(part))
		if err != nil {
			return nil, fmt.Errorf("the recipient %w; separate the recipients of a group with commas", err)
		}
		out = append(out, pk)
	}
	return out, nil
}

// plural gives one for n == 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// publicKey reads a public key of 64 hex characters, as arc whoami prints
// it. Its error names the first character that is not hex, or says that
// the hex is not a key.
func publicKey(text string) (nostr.PubKey, error) {
	if len(text) != 64 {
		return nostr.PubKey{}, fmt.Errorf("is not a key: a key is 64 hex characters, as arc whoami prints it, and this has %d", len(text))
	}
	for i, c := range text {
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return nostr.PubKey{}, fmt.Errorf("is not a key: %s at position %d is not hex", terminal.EscapeLine(strconv.QuoteRune(c)), i+1)
		}
	}
	pk, err := nostr.PubKeyFromHex(text)
	if err != nil {
		return nostr.PubKey{}, errors.New("is not a key: it is hex, but no key has this value; check it for a typo")
	}
	return pk, nil
}

func hexKeys(pks []nostr.PubKey) []string {
	out := make([]string, len(pks))
	for i, pk := range pks {
		out[i] = pk.Hex()
	}
	return out
}

// petnames names some keys, separated by commas.
// inboxNotes says, for each recipient, when the send found no relay list,
// or used an old one because no relay or indexer answered
// (docs/delivery/SPEC.md, section 3.8).
func inboxNotes(sess *citizen.Session, out []mail.Outgoing) {
	for _, o := range out {
		pk, err := nostr.PubKeyFromHex(o.To)
		if err != nil {
			continue
		}
		name := petnames([]nostr.PubKey{pk})
		switch o.Inbox {
		case mail.InboxNone:
			if len(sess.Relays) > 0 {
				fmt.Fprintf(os.Stderr, "no relay list for %s on your relays or indexers: the mail waits on your relays only; if %s reads mail on other relays, add one of them with arc relay add --index <url>, and arc sync sends it there\n", name, name)
			}
		case mail.InboxStored:
			fmt.Fprintf(os.Stderr, "no relay or indexer answered the lookup of the relay list of %s: the mail goes to the relays of the list in your store, which can be old\n", name)
		}
	}
}

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
			Members: members[o.Rumor], ReplyTo: o.ReplyTo,
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

// toTerminal reports whether w is a terminal.
func toTerminal(w io.Writer) bool {
	file, ok := w.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
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
		if m.ReplyTo != "" {
			group += "  reply to " + m.ReplyTo[:6]
		}
		_, err := fmt.Fprintf(p.out, "%s  %s%s\n  %s\n", m.At.Local().Format("2006-01-02 15:04"), keys.Name(m.From[:]), group, shown(m.Text)) //arc:raw-ok a petname that arc makes from the key; ReplyTo is 64 hex digits
		return err
	}
	return p.encoder.Encode(messageLine{
		ID: m.ID, From: m.From.Hex(), Name: keys.Name(m.From[:]),
		At: m.At.UTC().Format(time.RFC3339), Text: m.Text, Members: hexKeys(m.Members),
		ReplyTo: m.ReplyTo,
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
		pk, err := publicKey(text)
		if err != nil {
			return fmt.Errorf("--from %w", err)
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
	printer := newMessagePrinter(command.OutOrStdout(), sess.Signer.PublicKey(), asJSON)
	return sess.WatchInbox(ctx, citizen.InboxWatch{
		Since: since, From: senders, Show: printer.print,
		Log: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})),
	})
}
