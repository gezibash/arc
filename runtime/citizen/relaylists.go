package citizen

import (
	"context"
	"fmt"
	"io"
	"slices"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/relaylist"
	"github.com/gezibash/arc/core/transport"
	"github.com/gezibash/arc/sdk/terminal"
)

// PublishRelayList tells other citizens which relays this citizen reads and
// writes on, as NIP-65 defines, and which relays it reads its mail on, as
// NIP-17 defines. It returns the indexers that did not get each list.
func (sess *Session) PublishRelayList(ctx context.Context, errs io.Writer, quiet func() bool) []transport.Transport {
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
