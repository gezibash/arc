package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

func messageCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "message",
		Short: "Private messages, carried by relays or by hand",
		Long: "A message waits in the outbox until its recipient acknowledges it.\n" +
			"arc sync moves it: through relays, or through a directory that\n" +
			"someone carries. A machine that carries a directory also carries\n" +
			"other citizens' mail, which it cannot read.",
	}

	send := &cobra.Command{
		Use: "send <public key>[,<public key>...] [text...]", Short: "Send a message, to one citizen or to a group",
		Long: "With several public keys, separated by commas, arc sends one group\n" +
			"message: each member sees the others, and a reply goes to all of them.\n" +
			"A group has at most 10 participants, with you.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			to, err := recipients(args[0])
			if err != nil {
				return err
			}
			// A reply is checked before the text, so a wrong id does not
			// wait for standard input.
			parent, err := replyParent(command)
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			if parent != (nostr.ID{}) && testTags(command, to) == nil {
				if err := sess.Mail.CheckReply(command.Context(), to, parent); err != nil {
					return err
				}
			}

			text := strings.Join(args[1:], " ")
			if text == "" {
				body, err := io.ReadAll(io.LimitReader(os.Stdin, private.MaxMailContent+1))
				if err != nil {
					return err
				}
				text = strings.TrimRight(string(body), "\n")
			}
			if terminal.Blank(text) {
				return errors.New("a message holds at least one character that a person can see")
			}
			if len(text) > private.MaxMailContent {
				return fmt.Errorf("the message is too large for mail, which holds at most %d bytes: send it over a direct path, such as the transfer app", private.MaxMailContent)
			}

			out, err := sendMessage(command, sess, to, text)
			if err != nil {
				return err
			}
			inboxNotes(sess, out)
			if asJSON, _ := command.Flags().GetBool("json"); asJSON {
				if len(sess.Relays) == 0 {
					fmt.Fprintln(os.Stderr, "no relays: run arc sync --dir <path> to hand it to a courier")
				}
				reply, _ := command.Flags().GetString("reply")
				return json.NewEncoder(command.OutOrStdout()).Encode(queuedLine{
					ID: out[0].Rumor, To: out[0].To, State: out[0].State(time.Now()),
					Members: hexKeys(append([]nostr.PubKey{sess.Signer.PublicKey()}, to...)),
					ReplyTo: reply,
				})
			}
			fmt.Fprintf(command.OutOrStdout(), "queued for %s until %s\n", petnames(to), out[0].Expires.Local().Format("2006-01-02 15:04"))
			if len(sess.Relays) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no relays: run arc sync --dir <path> to hand it to a courier")
			}
			return nil
		},
	}

	inbox := &cobra.Command{
		Use: "inbox", Short: "List the messages you received", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			msgs, err := sess.Mail.Inbox(command.Context())
			if err != nil {
				return err
			}
			if err := printHolding(command, sess); err != nil {
				return err
			}
			asJSON, _ := command.Flags().GetBool("json")
			printer := newMessagePrinter(command.OutOrStdout(), sess.Signer.PublicKey(), asJSON)
			if asJSON {
				for _, m := range msgs {
					if err := printer.print(m); err != nil {
						return err
					}
				}
				return nil
			}
			if len(msgs) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no messages: run arc sync")
			}
			for _, m := range msgs {
				if err := printer.print(m); err != nil {
					return err
				}
			}
			n, err := sess.Mail.Carrying()
			if err != nil {
				return err
			}
			if n > 0 {
				fmt.Fprintf(command.OutOrStdout(), "\ncarrying %d sealed messages for other citizens\n", n)
			}
			return nil
		},
	}

	held := &cobra.Command{
		Use: "held", Short: "List the messages from strangers that arc held apart",
		Long: "A message from a key that is not a contact, without proof of work,\n" +
			"waits here, with no acknowledgement to its sender. It goes when its\n" +
			"wrap expires, after 7 days. arc contacts add <key> moves the messages\n" +
			"of that key to the inbox, and acknowledges them.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			msgs, err := sess.Mail.Held(command.Context())
			if err != nil {
				return err
			}
			asJSON, _ := command.Flags().GetBool("json")
			if len(msgs) == 0 && !asJSON {
				fmt.Fprintln(command.OutOrStdout(), "no held messages")
			}
			printer := newMessagePrinter(command.OutOrStdout(), sess.Signer.PublicKey(), asJSON)
			for _, m := range msgs {
				if err := printer.print(m); err != nil {
					return err
				}
			}
			return nil
		},
	}

	outbox := &cobra.Command{
		Use: "outbox", Short: "List the messages you sent, and whether they arrived", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			out, err := sess.Mail.Outbox(command.Context())
			if err != nil {
				return err
			}
			if asJSON, _ := command.Flags().GetBool("json"); asJSON {
				return printSent(command, sess.Mail, out)
			}
			if len(out) == 0 {
				fmt.Fprintln(command.OutOrStdout(), "no messages sent")
			}
			printOutbox(command.OutOrStdout(), out, time.Now())
			return nil
		},
	}

	send.Flags().Bool("json", false, "print the queued message as one JSON object")
	send.Flags().String("reply", "", "reply to the message with this id, 64 hex digits; the recipients are its members")
	addTestTags(send)
	inbox.Flags().Bool("json", false, "print one JSON object for each message")
	held.Flags().Bool("json", false, "print one JSON object for each message")
	outbox.Flags().Bool("json", false, "print one JSON object for each message, and no requests")
	command.AddCommand(send, inbox, held, outbox, messageWatchCommand())
	return command
}
