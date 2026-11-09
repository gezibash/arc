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
		RunE: func(command *cobra.Command, _ []string) error {
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
			sealed := sealedFilters(sess.Key.Public)
			repair, _ := command.Flags().GetBool("repair")
			turn, _ := command.Flags().GetBool("turn")
			if turn && !repair {
				return errors.New("--turn allows TURN for repair: give it with --repair")
			}

			targets := sess.Relays
			dir, _ := command.Flags().GetString("dir")
			if dir != "" {
				targets = []transport.Transport{file.Dir{Path: dir}}
			}
			if len(targets) == 0 {
				return errors.New("nothing to sync with: add a relay, or give --dir")
			}

			if dir != "" {
				refreshDevices(command.Context(), sess, command.ErrOrStderr())
			}
			failed := false
			// unreached has one plain line for each relay that gave no
			// answer or refused, and no line of Go text (#442, #463).
			var unreached []string
			// Ask each relay once, all at once: a relay that does not answer
			// is then down, and the steps below skip it, so silent relays
			// cost one deadline in total, not one each (#442).
			reached := map[string]error{}
			if dir == "" {
				for i, err := range sess.Reach(command.Context()) {
					reached[sess.Relays[i].Name()] = err
				}
			}
			for _, t := range targets {
				if err := reached[t.Name()]; err != nil && citizen.Unreachable(err) {
					unreached = append(unreached, t.Name()+": "+citizen.UnreachedReason(err))
					continue
				}
				if line, ok := citizen.RefusedLine(t.Name(), reached[t.Name()]); ok {
					unreached = append(unreached, line)
					continue
				}
				// Self data goes to a directory that the citizen carries,
				// but never to a relay.
				if dir != "" {
					report, err := syncSealed(command.Context(), sess.Node, sealed, noContacts{t})
					if err != nil {
						fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s: %v", t.Name(), err)))
						failed = true
						continue
					}
					fmt.Fprintf(command.OutOrStdout(), "%s: sealed data received %d, sent %d, already held %d\n",
						terminal.EscapeLine(report.Transport), report.Received, report.Sent, report.Duplicate+report.Superseded)
					for _, reason := range report.Refused {
						fmt.Fprintf(os.Stderr, "  refused %s\n", terminal.EscapeLine(reason))
					}
					if err := dropDeleted(command.Context(), command.OutOrStdout(), sess.Node, sealed, file.Dir{Path: dir}, report); err != nil {
						fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("%s: the events of deleted pages stay: %v", t.Name(), err)))
						failed = true
					}
					if report.Unreadable > 0 {
						fmt.Fprintf(os.Stderr, "  %d items did not parse\n", report.Unreadable)
					}
					for _, err := range report.SendFailed {
						fmt.Fprintf(os.Stderr, "  not sent: %s\n", terminal.EscapeLine(err.Error()))
					}
					bodies, err := sess.SyncBodies(dir)
					if err != nil {
						fmt.Fprintf(os.Stderr, "%s: bodies: %s\n", terminal.EscapeLine(t.Name()), terminal.EscapeLine(err.Error()))
						failed = true
					} else if bodies.Sent+bodies.Received+len(bodies.Missing) > 0 {
						fmt.Fprintf(command.OutOrStdout(), "%s: bodies received %d, sent %d\n", terminal.EscapeLine(report.Transport), bodies.Received, bodies.Sent)
						for _, sum := range bodies.Missing {
							fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("  the body %s is on neither side; the device that wrote it holds it", sum)))
						}
					}
				}

				if offers, err := sess.Node.Sync(command.Context(), announcements, t); err == nil && offers.Received > 0 {
					fmt.Fprintf(command.OutOrStdout(), "%s: announcements received %d\n", terminal.EscapeLine(t.Name()), offers.Received)
				}

				mails, err := sess.Mail.Sync(command.Context(), t)
				if line, ok := citizen.StoppedLine(terminal.EscapeLine(t.Name()), err); ok {
					// The relay took some wraps, and then gave no answer:
					// one line names it, and the rest wait (#465).
					unreached = append(unreached, line)
					continue
				}
				if err != nil && citizen.Unreachable(err) {
					// The relay answered the question of Reach, so only
					// its mail gave no answer: say so, in a few words at
					// the end, not its whole error (#439, #442).
					unreached = append(unreached, t.Name()+": "+citizen.UnreachedReason(err)+" for mail")
					continue
				}
				if line, ok := citizen.RefusedLine(t.Name()+" for mail", err); ok {
					unreached = append(unreached, line)
					continue
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "%s: mail: %s\n", terminal.EscapeLine(t.Name()), terminal.EscapeLine(err.Error()))
					failed = true
					continue
				}
				fmt.Fprintf(command.OutOrStdout(), "%s: mail received %d, delivered %d, carried %d, sent %d\n",
					terminal.EscapeLine(mails.Transport), mails.Received, mails.Delivered, mails.Carried, mails.Sent)
				for _, reason := range mails.Refused {
					fmt.Fprintf(os.Stderr, "  refused %s\n", terminal.EscapeLine(reason))
				}
			}
			if dir == "" && len(sess.Relays) > 0 {
				// A relay that gave no answer is named once at the end.
				if sent, err := sess.Mail.Forward(command.Context()); err != nil && !citizen.Unusable(err) {
					fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("mail: the waiting messages were not sent to the relays of their recipients: %v", err)))
					failed = true
				} else if sent > 0 {
					fmt.Fprintf(command.OutOrStdout(), "mail: sent %d waiting %s to the relays that %s recipients read\n", sent, plural(sent, "message", "messages"), plural(sent, "its", "their"))
				}
			}
			if dir == "" {
				cfg, err := selfSyncConfig(command, sess)
				if err != nil {
					return err
				}
				again, _ := command.Flags().GetBool("import")
				cfg.Repair, cfg.RepairTURN = repair, turn
				if syncSelf(command.Context(), sess, cfg, again) != nil {
					failed = true
				}
			}
			// One line for each relay that gave no answer, the last as the
			// error of the sync when nothing else failed. A line holds raw
			// text that a relay chose, and is escaped once, where it is
			// printed: errorLine escapes the error of the command (#464).
			for i, relay := range unreached {
				if i < len(unreached)-1 || failed {
					fmt.Fprintln(command.ErrOrStderr(), "arc: "+terminal.EscapeLine(relay+"; arc syncs with it again at the next sync"))
				} else {
					return errors.New(relay + "; arc syncs with it again at the next sync")
				}
			}
			if failed {
				return errors.New("some transports failed")
			}
			return nil
		},
	}
	command.Flags().String("dir", "", "sync with this directory instead of the relays")
	command.Flags().Bool("import", false, "import the drafts that the relays hold again")
	command.Flags().Bool("repair", false, "audit the pieces on the storage citizens, and rebuild, move or delete pieces; arc serve does this on each tick")
	command.Flags().Bool("turn", false, "with --repair, let repair use a pipe through TURN; it waits for a direct path by default")
	return command
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
