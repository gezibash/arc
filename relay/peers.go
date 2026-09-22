package relay

import (
	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/identity"
)

// The relay side of a federation link. The federation carries the bytes; the
// relay decides what to do with them.

// peerUp runs when a link to a partner becomes ready. The catalog of that
// partner is read from the start.
func (r *Relay) peerUp(peer []byte) {
	r.log.Info("the peer link is ready", "peer", identity.Name(peer))
	r.catalog.peerUp(peer)
}

// forgetPeer drops what a partner gave us. A record that only that partner
// knew goes with it.
func (r *Relay) forgetPeer(peer []byte) {
	r.log.Debug("the peer link ended", "peer", identity.Name(peer))
	r.catalog.forget(peer)
}

// answerPeer answers one control request of a partner.
func (r *Relay) answerPeer(peer []byte, request map[string]any) map[string]any {
	switch request["type"] {
	case "catalog":
		return r.catalog.answer(peer, request)

	default:
		return map[string]any{"ok": false, "error": "invalid_request"}
	}
}

// carryFromPeer takes one packet that a partner forwarded. The destination
// must be a citizen of this relay, and its publication must allow it.
func (r *Relay) carryFromPeer(peer, raw []byte) {
	decoded, err := decodeForPeer(raw)
	if err != nil {
		r.drop(raw, "the packet of a peer is not valid")
		return
	}

	// A partner may only reach a citizen that shares beyond this relay, or
	// one that is waiting for the answer of its own request.
	shared := r.sharesBeyond(decoded.Dst)
	if !shared && !r.allowedReply(decoded, [][]byte{r.identity.PublicKey, peer}) {
		r.drop(raw, "the destination does not take traffic from a peer")
		return
	}

	r.mu.RLock()
	destination := r.routes[string(decoded.Dst)]
	r.mu.RUnlock()

	if destination == nil {
		r.drop(raw, "no route")
		return
	}
	if !destination.send(raw) {
		r.drop(raw, "the destination is behind")
		return
	}

	// The answer of this request travels back over the same link.
	if shared {
		r.remember(decoded.Src, decoded.Dst, decoded.SessionID,
			[][]byte{r.identity.PublicKey, peer})
	}
}

// sharesBeyond says whether a citizen of this relay offers its capability
// past this relay.
func (r *Relay) sharesBeyond(citizen []byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	held, there := r.directory[string(citizen)]
	return there && held.entry.Federatable(r.identity.PublicKey)
}

// homeOf names the relay that a record calls home.
func homeOf(entry *announce.Entry) []byte {
	if entry == nil || entry.Federation == announce.Local {
		return nil
	}
	return entry.RelayPublicKey
}
