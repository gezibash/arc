package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/transport/file"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func syncCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "sync",
		Short: "Reconcile this machine with its relays and storage citizens, or with a directory",
		Long: "Without --dir, arc syncs mail and announcements with every relay, and\n" +
			"self data, such as the journal, with each storage citizen. Self data\n" +
			"never goes to a relay. The first sync imports the drafts that the\n" +
			"relays still hold; --import runs that again. With --dir, arc syncs\n" +
			"everything with that directory: a USB stick, a shared folder, or a\n" +
			"disk you carry.",
		Args: cobra.NoArgs,
		RunE: runSync,
	}
	command.Flags().String("dir", "", "sync with this directory instead of the relays")
	command.Flags().Bool("import", false, "import the drafts that the relays hold again")
	command.Flags().Bool("repair", false, "audit the pieces on the storage citizens, and rebuild, move or delete pieces; arc serve does this on each tick")
	command.Flags().Bool("turn", false, "with --repair, let repair use a pipe through TURN; it waits for a direct path by default")
	return command
}

// syncRun is one run of arc sync: what it syncs with, and what went wrong.
type syncRun struct {
	command *cobra.Command
	sess    *citizen.Session
	sealed  []nostr.Filter
	dir     string
	failed  bool
	// unreached has one plain line for each relay that gave no
	// answer or refused, and no line of Go text (#442, #463).
	unreached []string
}

// runSync runs arc sync: it reaches each relay once, syncs each target,
// forwards waiting mail and syncs self data, then names what failed.
func runSync(command *cobra.Command, _ []string) error {
	// A directory holds events, not the pieces of a storage citizen,
	// so a repair through it has nothing to audit (#404).
	if repair, _ := command.Flags().GetBool("repair"); repair {
		if dir, _ := command.Flags().GetString("dir"); dir != "" {
			return errors.New("--repair audits the pieces on the storage citizens, and --dir syncs with a directory, which holds no pieces: run arc sync --repair and arc sync --dir apart")
		}
	}
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	repair, _ := command.Flags().GetBool("repair")
	turn, _ := command.Flags().GetBool("turn")
	if turn && !repair {
		return errors.New("--turn allows TURN for repair: give it with --repair")
	}

	sync := &syncRun{command: command, sess: sess, sealed: sealedFilters(sess.Key.Public)}
	targets := sess.Relays
	sync.dir, _ = command.Flags().GetString("dir")
	if sync.dir != "" {
		targets = []transport.Transport{file.Dir{Path: sync.dir}}
	}
	if len(targets) == 0 {
		return errors.New("nothing to sync with: add a relay, or give --dir")
	}
	if sync.dir != "" {
		refreshDevices(command.Context(), sess, command.ErrOrStderr())
	}
	// Ask each relay once, all at once: a relay that does not answer
	// is then down, and the steps below skip it, so silent relays
	// cost one deadline in total, not one each (#442).
	reached := map[string]error{}
	if sync.dir == "" {
		for i, err := range sess.Reach(command.Context()) {
			reached[sess.Relays[i].Name()] = err
		}
	}
	for _, t := range targets {
		sync.target(t, reached[t.Name()])
	}
	if sync.dir == "" && len(sess.Relays) > 0 {
		sync.forward()
	}
	if sync.dir == "" {
		cfg, err := selfSyncConfig(command, sess)
		if err != nil {
			return err
		}
		again, _ := command.Flags().GetBool("import")
		cfg.Repair, cfg.RepairTURN = repair, turn
		if syncSelf(command.Context(), sess, cfg, again) != nil {
			sync.failed = true
		}
	}
	return sync.finish()
}

// target syncs with one relay or directory. reached is the answer of the
// relay to Reach.
func (r *syncRun) target(t transport.Transport, reached error) {
	command, sess := r.command, r.sess
	if reached != nil && citizen.Unreachable(reached) {
		r.unreached = append(r.unreached, citizen.UnreachedLine(t.Name(), reached))
		return
	}
	if line, ok := citizen.RefusedLine(t.Name(), reached); ok {
		r.unreached = append(r.unreached, line)
		return
	}
	// Self data goes to a directory that the citizen carries,
	// but never to a relay.
	if r.dir != "" && !r.directory(t) {
		return
	}

	if offers, err := sess.Node.Sync(command.Context(), announcements, t); err == nil && offers.Received > 0 {
		fmt.Fprintf(command.OutOrStdout(), "%s: announcements received %d\n", terminal.EscapeLine(t.Name()), offers.Received)
	}
	r.mail(t)
}

// directory syncs the self data and the bodies with a directory. It
// returns false when the sealed data did not sync, and the target stops.
func (r *syncRun) directory(t transport.Transport) bool {
	command, sess := r.command, r.sess
	report, err := syncSealed(command.Context(), sess.Node, r.sealed, noContacts{t})
	if err != nil {
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s: %v", t.Name(), err)))
		r.failed = true
		return false
	}
	fmt.Fprintf(command.OutOrStdout(), "%s: sealed data received %d, sent %d, already held %d\n",
		terminal.EscapeLine(report.Transport), report.Received, report.Sent, report.Duplicate+report.Superseded)
	for _, reason := range report.Refused {
		fmt.Fprintf(os.Stderr, "  refused %s\n", terminal.EscapeLine(reason))
	}
	if err := dropDeleted(command.Context(), command.OutOrStdout(), sess.Node, r.sealed, file.Dir{Path: r.dir}, report); err != nil {
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s: the events of deleted pages stay: %v", t.Name(), err)))
		r.failed = true
	}
	if report.Unreadable > 0 {
		fmt.Fprintf(os.Stderr, "  %d items did not parse\n", report.Unreadable)
	}
	for _, err := range report.SendFailed {
		fmt.Fprintf(os.Stderr, "  not sent: %s\n", terminal.EscapeLine(err.Error()))
	}
	bodies, err := sess.SyncBodies(r.dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: bodies: %s\n", terminal.EscapeLine(t.Name()), terminal.EscapeLine(err.Error()))
		r.failed = true
	} else if bodies.Sent+bodies.Received+len(bodies.Missing) > 0 {
		fmt.Fprintf(command.OutOrStdout(), "%s: bodies received %d, sent %d\n", terminal.EscapeLine(report.Transport), bodies.Received, bodies.Sent)
		for _, sum := range bodies.Missing {
			fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("  the body %s is on neither side; the device that wrote it holds it", sum)))
		}
	}
	return true
}

// mail syncs the mail with one target.
func (r *syncRun) mail(t transport.Transport) {
	command := r.command
	mails, err := r.sess.Mail.Sync(command.Context(), t)
	if line, ok := citizen.StoppedLine(t.Name(), err); ok {
		// The relay took some wraps, and then gave no answer:
		// one line names it, and the rest wait (#465).
		r.unreached = append(r.unreached, line)
		return
	}
	if err != nil && citizen.Unreachable(err) {
		// The relay answered the question of Reach, so only
		// its mail gave no answer: say so, in a few words at
		// the end, not its whole error (#439, #442).
		r.unreached = append(r.unreached, t.Name()+": "+citizen.UnreachedReason(err)+" for mail; arc syncs with it again at the next sync")
		return
	}
	if line, ok := citizen.RefusedLine(t.Name()+" for mail", err); ok {
		r.unreached = append(r.unreached, line)
		return
	}
	if err != nil {
		fmt.Fprintf(command.ErrOrStderr(), "%s: mail: %s\n", terminal.EscapeLine(t.Name()), terminal.EscapeLine(err.Error()))
		r.failed = true
		return
	}
	fmt.Fprintf(command.OutOrStdout(), "%s: mail received %d, delivered %d, carried %d, sent %d\n",
		terminal.EscapeLine(mails.Transport), mails.Received, mails.Delivered, mails.Carried, mails.Sent)
	for _, reason := range mails.Refused {
		fmt.Fprintf(os.Stderr, "  refused %s\n", terminal.EscapeLine(reason))
	}
}

// forward sends the waiting mail to the relays of its recipients. A relay
// that gave no answer is named once at the end.
func (r *syncRun) forward() {
	command := r.command
	if sent, err := r.sess.Mail.Forward(command.Context()); err != nil && !citizen.Unusable(err) {
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("mail: the waiting messages were not sent to the relays of their recipients: %v", err)))
		r.failed = true
	} else if sent > 0 {
		fmt.Fprintf(command.OutOrStdout(), "mail: sent %d waiting %s to the relays that %s recipients read\n", sent, plural(sent, "message", "messages"), plural(sent, "its", "their"))
	}
}

// finish writes the line of each relay that gave no answer or refused.
func (r *syncRun) finish() error {
	return relayLines(r.command.ErrOrStderr(), r.unreached, r.failed)
}

// relayLines prints the line of sync for each relay that gave no answer or
// refused, the last as the error of the sync when nothing else failed. A
// line holds raw text that a relay or a mark chose, with its own ending,
// and is escaped once, where it is printed: errorLine escapes the error of
// the command (#444, #464).
func relayLines(errs io.Writer, lines []string, failed bool) error {
	for i, line := range lines {
		if i < len(lines)-1 || failed {
			fmt.Fprintln(errs, "arc: "+terminal.EscapeLine(line))
		} else {
			return errors.New(line)
		}
	}
	if failed {
		return errors.New("some transports failed")
	}
	return nil
}

// sealedFilters match the self data of a citizen: drafts, their checkpoints
// and parts, and deletion requests. The first matches what the citizen key
// signed. The second matches what a device key signed, by the p tag that
// names the citizen.
func sealedFilters(me nostr.PubKey) []nostr.Filter {
	kinds := []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind, nostr.KindDeletion}
	return []nostr.Filter{
		{Kinds: kinds, Authors: []nostr.PubKey{me}},
		{Kinds: kinds, Tags: nostr.TagMap{"p": {me.Hex()}}},
	}
}

// syncSealed syncs each filter of self data with one transport, and adds
// the reports.
func syncSealed(ctx context.Context, n *node.Node, filters []nostr.Filter, t transport.Transport) (node.Report, error) {
	var sum node.Report
	// An event can match two filters. It counts once.
	deleted := map[nostr.ID]bool{}
	for _, filter := range filters {
		report, err := n.Sync(ctx, filter, t)
		if err != nil {
			return sum, err
		}
		sum.Transport = report.Transport
		sum.Received += report.Received
		sum.Duplicate += report.Duplicate
		sum.Superseded += report.Superseded
		sum.Refused = append(sum.Refused, report.Refused...)
		for _, id := range report.Deleted {
			if !deleted[id] {
				deleted[id] = true
				sum.Deleted = append(sum.Deleted, id)
			}
		}
		sum.Unreadable += report.Unreadable
		sum.Sent += report.Sent
		for _, err := range report.SendFailed {
			if !errors.Is(err, errContactDraft) {
				sum.SendFailed = append(sum.SendFailed, err)
			}
		}
	}
	return sum, nil
}

// dropDeleted removes from a directory each event that the store refused
// because its author deleted it, and says how many. Another device learns
// of the delete from the directory, so it removes nothing unless the
// directory holds a valid copy of each deletion request of the store.
func dropDeleted(ctx context.Context, out io.Writer, n *node.Node, filters []nostr.Filter, d file.Dir, report node.Report) error {
	if len(report.Deleted) == 0 {
		return nil
	}
	held, err := holdsDeletions(ctx, n, filters, d)
	if err != nil {
		return err
	}
	if !held {
		fmt.Fprintf(out, "%s: %d events of deleted pages left out; they stay in the directory, because it lacks a valid copy of a deletion request\n", terminal.EscapeLine(report.Transport), len(report.Deleted))
		return nil
	}
	removed, err := d.Remove(report.Deleted)
	fmt.Fprintf(out, "%s: %d events of deleted pages left out, %d removed from the directory\n", terminal.EscapeLine(report.Transport), len(report.Deleted), removed)
	return err
}

// holdsDeletions says whether the directory holds each deletion request
// of the store that the filters match, and whether each copy verifies. A
// changed copy keeps the ID of the request, so the sync sent no valid one.
func holdsDeletions(ctx context.Context, n *node.Node, filters []nostr.Filter, d file.Dir) (bool, error) {
	want := map[nostr.ID]bool{}
	for _, filter := range filters {
		filter.Kinds = []nostr.Kind{nostr.KindDeletion}
		found, err := n.Store.Query(filter)
		if err != nil {
			return false, err
		}
		for _, e := range found {
			want[e.ID] = true
		}
	}
	if len(want) == 0 {
		return false, nil
	}
	batch, err := d.Fetch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindDeletion}})
	if err != nil {
		return false, err
	}
	for _, e := range batch.Events {
		if want[e.ID] && store.Verify(e) == nil {
			delete(want, e.ID)
		}
	}
	return len(want) == 0, nil
}

// errContactDraft says that a directory gets no draft of the contact list.
var errContactDraft = errors.New("a draft of the contact list stays off a directory")

// noContacts gives a directory each event but the drafts of the contact
// list. A stick can reach a courier, and such a draft names the citizen.
type noContacts struct{ transport.Transport }

func (t noContacts) Send(ctx context.Context, event nostr.Event) error {
	if citizen.ContactDraft(event) {
		return errContactDraft
	}
	return t.Transport.Send(ctx, event)
}
