// Package relaylist makes and reads the relay list of a citizen, as NIP-65
// defines: kind 10002, with one r tag for each relay.
//
// A citizen writes its public events to its write relays, and reads what
// others send it on its read relays. An r tag without a marker names a relay
// for both. See docs/delivery/SPEC.md, sections 7.2 and 11.4.
package relaylist

import (
	"context"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/transport"
)

// Kind is the relay list of NIP-65.
const Kind nostr.Kind = 10002

// Make signs a relay list that names each relay for both reading and
// writing.
func Make(k keys.Signer, urls []string, at nostr.Timestamp) (nostr.Event, error) {
	tags := nostr.Tags{}
	for _, url := range urls {
		tags = append(tags, nostr.Tag{"r", url})
	}
	event := nostr.Event{Kind: Kind, CreatedAt: at, Tags: tags}
	if err := k.Sign(&event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// Read returns the read relays and the write relays of a list.
func Read(list nostr.Event) (read, write []string) {
	for _, t := range list.Tags {
		if len(t) < 2 || t[0] != "r" || t[1] == "" {
			continue
		}
		marker := ""
		if len(t) > 2 {
			marker = t[2]
		}
		if marker != "write" {
			read = append(read, t[1])
		}
		if marker != "read" {
			write = append(write, t[1])
		}
	}
	return read, write
}

// ReadRelays returns the read relays of a citizen, from the newest list in
// the store. When the store holds none, it asks the transports first.
func ReadRelays(ctx context.Context, n *node.Node, who nostr.PubKey, via []transport.Transport) []string {
	filter := nostr.Filter{Kinds: []nostr.Kind{Kind}, Authors: []nostr.PubKey{who}}
	lists := n.Store.Query(filter)
	if len(lists) == 0 && len(via) > 0 {
		n.Pull(ctx, filter, via)
		lists = n.Store.Query(filter)
	}
	if len(lists) == 0 {
		return nil
	}
	read, _ := Read(lists[0])
	return read
}

// Same says whether two relay URLs name one relay. It ignores a trailing
// slash and the case of the scheme and host.
func Same(a, b string) bool {
	return strings.EqualFold(strings.TrimRight(a, "/"), strings.TrimRight(b, "/"))
}
