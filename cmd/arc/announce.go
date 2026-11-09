package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"sync/atomic"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/relaylist"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// publishRelayList tells other citizens which relays this citizen reads and
// writes on, as NIP-65 defines, and which relays it reads its mail on, as
// NIP-17 defines. It returns the indexers that did not get each list.
func publishRelayList(ctx context.Context, sess *citizen.Session, errs io.Writer, quiet func() bool) []transport.Transport {
	// The lists go to the relays and to the indexers.
	type list struct {
		event nostr.Event
		to    []transport.Transport
	}
	public := slices.Concat(sess.Relays, sess.Indexers)
	var lists []list
	outbox, err := relaylist.Make(ctx, sess.Signer, sess.URLs, nostr.Now())
	if err != nil {
		fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("the NIP-65 relay list was not signed: %v", err)))
	} else {
		lists = append(lists, list{outbox, public})
	}
	inbox, err := mail.RelayList(ctx, sess.Signer, sess.URLs, nostr.Now())
	if err != nil {
		fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("the relay list was not signed: %v", err)))
	} else {
		lists = append(lists, list{inbox, public})
	}
	var owed []transport.Transport
	for _, l := range lists {
		_, sent, _ := sess.Node.Publish(ctx, l.event, l.to)
		for i, s := range sent {
			if s.Err != nil {
				if quiet == nil || !quiet() {
					fmt.Fprintln(errs, terminal.EscapeLine(fmt.Sprintf("the relay list did not reach %s: %v", s.Transport, s.Err)))
				}
				// The relays come first in l.to, and the indexers after them.
				if i >= len(sess.Relays) && !slices.Contains(owed, l.to[i]) {
					owed = append(owed, l.to[i])
				}
			}
		}
	}
	return owed
}

func announceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "announce <manifest.json>",
		Short: "Announce a capability of interface version 1 as its author",
		Long: "A data capability has no provider. Its author announces its manifest,\n" +
			"and citizens install it by trusting that author.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			m, err := iface.Parse(data)
			if err != nil {
				return err
			}
			// A new announcement replaces the old one only if it is newer.
			// The relays can hold one that this store does not.
			mine := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{sess.Key.Public}, Tags: nostr.TagMap{"d": {m.ID}}}
			// With no answer, arc cannot know the newest announcement: one
			// of the same second would tie, and the older could win (#422).
			// With no relay at all, no relay answers either (#445).
			if len(sess.Relays) == 0 {
				return fmt.Errorf("no relay answered, so arc cannot know the newest announcement of %s that the relays hold: announce it when a relay answers: this citizen has no relay; add one with arc relay add", terminal.EscapeLine(m.ID))
			}
			if err := node.Unreached(sess.Node.Pull(command.Context(), mine, sess.Relays)); err != nil {
				return fmt.Errorf("no relay answered, so arc cannot know the newest announcement of %s that the relays hold: announce it when a relay answers: %w", terminal.EscapeLine(m.ID), err)
			}
			at, err := catalog.After(sess.Node.Store, sess.Key.Public, m.ID, nostr.Now())
			if err != nil {
				return err
			}
			announcement, err := catalog.AnnounceManifest(command.Context(), sess.Signer, data, at)
			if err != nil {
				return err
			}
			if _, sent, err := sess.Node.Publish(command.Context(), announcement, sess.Relays); err != nil {
				return err
			} else {
				for _, s := range sent {
					if s.Err != nil {
						fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("not sent to %s: %v", s.Transport, s.Err)))
					}
				}
			}
			fmt.Fprintf(command.OutOrStdout(), "announced %s as %s\n%s\n", announcement.Tags.GetD(), sess.Key.Name(), sess.Key.Public.Hex()) //arc:raw-ok the name of a key of this machine
			return nil
		},
	}
}

// announcer signs the announcements of arc serve. Each is at least a second
// newer than the newest that the store holds, so that it never ties with an
// older one of the same second (#422). Until a relay answers for the
// announcements that the relays hold, arc cannot know the newest: it signs
// nothing, and says so once.
type announcer struct {
	// known and ahead are read by the watches of other goroutines.
	known, ahead atomic.Bool
	told         bool
	// pull asks the relays for the announcements, and is nil when one
	// answered.
	pull func() error
	// at is the time to sign at, after the newest in the store.
	at   func() (nostr.Timestamp, error)
	sign func(at nostr.Timestamp) error
	say  func(text string)
}

func (a *announcer) announce() error {
	if !a.known.Load() {
		a.known.Store(a.pull() == nil)
	}
	if !a.known.Load() {
		if !a.told {
			a.say("is not announced yet: no relay has answered, so arc cannot know the newest announcement; it asks again")
			a.told = true
		}
		return nil
	}
	at, err := a.at()
	// An announcement far ahead of this clock, from a wrong clock or a
	// hostile relay, holds the new one back, but arc serve keeps serving:
	// to stop would turn it into an outage. It says so once (#445).
	if ahead, ok := errors.AsType[*catalog.AheadError](err); ok {
		if !a.ahead.Load() {
			a.say(fmt.Sprintf("is not announced: an announcement is %s ahead of this clock; arc announces again in %s", ahead.Ahead, ahead.Ahead-catalog.MaxSkew))
		}
		a.ahead.Store(true)
		return nil
	}
	a.ahead.Store(false)
	if err != nil {
		return err
	}
	return a.sign(at)
}

// waiting says whether the announcer waits: for a relay that answers, or
// for the clock to reach an announcement far ahead of it.
func (a *announcer) waiting() bool { return !a.known.Load() || a.ahead.Load() }

// waitingWriter drops what a logger writes while waiting says so.
type waitingWriter struct {
	to      io.Writer
	waiting func() bool
}

func (w waitingWriter) Write(p []byte) (int, error) {
	if w.waiting() {
		return len(p), nil
	}
	return w.to.Write(p)
}
