package main

import (
	"fmt"
	"os"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/iface/manifest"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

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

			m, err := manifest.Parse(data)
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
