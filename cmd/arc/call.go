package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/gezibash/arc/runtime/iface"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// findOffer reads an offer from the store, and asks the relays when the store
// has none.
func callCmd() *cobra.Command {
	command := &cobra.Command{
		Use:   "call <provider|address> [body...]",
		Short: "Call an installed app service",
		Long: "Name the capability by an address, <scheme>+arc://<provider>/<path>, or\n" +
			"by its provider and --capability. The provider is a key, an npub, or an\n" +
			"installed name.\n\n" +
			"With a relay, the call is live: it needs the provider to be present\n" +
			"now, and it prints the reply and the round-trip time. With --later, or\n" +
			"with no relay, the call waits in the outbox and travels like a message;\n" +
			"arc call results shows the reply once a sync brings it.",
		Args: cobra.MinimumNArgs(1),
		RunE: callCapability,
	}
	command.Flags().Bool("later", false, "store and forward the call, even when a relay is there")
	command.Flags().String("capability", "", "the capability of the provider to call")
	command.Flags().String("method", "", "the method of the call (default: the manifest's)")
	command.Flags().String("path", "", "the path of the call (default: the manifest's)")
	command.Flags().Duration("timeout", call.Timeout, "how long a live call waits")
	command.Flags().Bool("raw", false, "write the reply as it came, not as the manifest shows it")

	command.AddCommand(&cobra.Command{
		Use: "results", Short: "Show your store-and-forward calls, and their replies", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()

			found := false
			now := time.Now()
			out, err := sess.Mail.Outbox(command.Context())
			if err != nil {
				return err
			}
			for _, o := range out {
				if o.Kind != "request" {
					continue
				}
				found = true
				to, _ := nostr.PubKeyFromHex(o.To)
				fmt.Fprintf(command.OutOrStdout(), "%s  to %s  %s\n  %s\n", o.Created.Local().Format("2006-01-02 15:04"), keys.Name(to[:]), o.State(now), shown(o.Text)) //arc:raw-ok a petname that arc makes from the key; a state of the outbox
				switch {
				case o.Reply.Err != "":
					fmt.Fprintf(command.OutOrStdout(), "  error: %s\n", shown(o.Reply.Err))
				case o.ReplySeal != "":
					fmt.Fprintf(command.OutOrStdout(), "  reply: %s\n", shown(o.Reply.Body))
				}
			}
			if !found {
				fmt.Fprintln(command.OutOrStdout(), "no store-and-forward calls")
			}
			return nil
		},
	})
	return command
}

func callCapability(command *cobra.Command, args []string) error {
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()

	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	flag, _ := command.Flags().GetString("capability")
	provider, offer, addressPath, err := sess.Target(command.Context(), installs, args[0], flag)
	if err != nil {
		return err
	}
	trusted, err := installs.Trusted(provider, offer.ID)
	if err != nil {
		return err
	}
	if !trusted {
		return fmt.Errorf("install it first: arc install %s %s", provider.Hex(), offer.ID)
	}

	if !offer.SupportsInteraction(session.RequestReply) {
		return fmt.Errorf("%s does not support request/reply: %w", offer.ID, session.ErrUnsupported)
	}
	body := strings.Join(args[1:], " ")
	if body == "" {
		read, err := io.ReadAll(io.LimitReader(os.Stdin, 1024*1024+1))
		if err != nil {
			return err
		}
		body = string(read)
	}

	request := call.Request{Capability: offer.ID, Method: offer.Method, Path: offer.Path, Body: body}
	if method, _ := command.Flags().GetString("method"); method != "" {
		request.Method = strings.ToUpper(method)
	}
	if addressPath != "" {
		request.Path = addressPath
	}
	if path, _ := command.Flags().GetString("path"); path != "" {
		if addressPath != "" && path != addressPath {
			return fmt.Errorf("the address names the path %s, and --path names %s", addressPath, path)
		}
		request.Path = path
	}

	later, _ := command.Flags().GetBool("later")
	if later || len(sess.Relays) == 0 {
		if _, err := sess.Mail.Request(command.Context(), provider, request); err != nil {
			return err
		}
		fmt.Fprintf(command.OutOrStdout(), "queued for %s: the reply arrives with a sync; see arc call results\n", offer.Name()) //arc:raw-ok a petname that arc makes from the key
		return nil
	}

	timeout, _ := command.Flags().GetDuration("timeout")
	reply, rtt, via, err := sess.LiveCall(command.Context(), provider, request, timeout)
	if err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("round trip %s via %s", rtt.Round(100*time.Microsecond), via)))
	if reply.Err != "" {
		return fmt.Errorf("the provider refused: %s", reply.Err)
	}
	// The manifest can say how to show a reply. --raw writes it as it came.
	raw, _ := command.Flags().GetBool("raw")
	if m := offer.Manifest; !raw && m != nil && m.Service != nil && m.Service.Output != nil {
		env := &citizen.Environment{Session: sess, Installs: installs}
		in := iface.Installed{Manifest: m, Author: provider, Name: offer.ID}
		return iface.ShowReply(command.Context(), env, in, reply.Body, iface.Stdio{In: os.Stdin, Out: command.OutOrStdout(), Err: os.Stderr})
	}
	// The reply is escaped wherever it goes: a program or an agent that
	// reads it through a pipe has no terminal, and gets the hidden
	// characters too (docs/clients/SPEC.md, section 3.9). Through a pipe,
	// JSON stays valid JSON. --raw writes the reply as it came.
	text := reply.Body
	switch out := command.OutOrStdout(); {
	case raw:
		if toTerminal(out) {
			fmt.Fprintln(command.ErrOrStderr(), "arc: --raw writes the reply as it came, and it can change this terminal")
		}
	case toTerminal(out):
		text = terminal.Escape(text)
	case json.Valid([]byte(text)):
		text = terminal.EscapeJSON(text)
	default:
		text = terminal.EscapeData(text)
	}
	fmt.Fprint(command.OutOrStdout(), text) //arc:raw-ok --raw writes the reply as it came; each other case escapes it
	if !strings.HasSuffix(text, "\n") {
		fmt.Fprintln(command.OutOrStdout())
	}
	return nil
}
