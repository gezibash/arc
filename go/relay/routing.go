package relay

import (
	"encoding/hex"
	"time"

	"github.com/gezibash/arc/go/announce"
	"github.com/gezibash/arc/go/packet"
)

// A packet for a citizen of another relay travels one of two ways.
//
// A partner that holds the destination takes the packet as it is, on the
// link between the two relays.
//
// A citizen further away needs a path. The relay of the caller wraps the
// packet with the relays on the way and the signed announcement of the
// provider. Each relay on the way checks its own place in the path, that
// the packet came from the relay before it, and that its operator allows
// traffic to pass. The reply travels the same path, backwards.

// conversation is a reply that a relay lets through, and the way back.
type conversation struct {
	path      [][]byte
	expiresAt time.Time
}

// decodeForPeer reads a packet that arrived on a link.
func decodeForPeer(raw []byte) (*packet.Packet, error) {
	decoded, err := packet.Decode(raw)
	if err != nil {
		return nil, err
	}
	if !fresh(decoded) {
		return nil, ErrPeerUnavailable
	}
	return decoded, nil
}

// carryAway sends a packet that no citizen of this relay holds. It returns
// false when there is no way to the destination.
func (r *Relay) carryAway(decoded *packet.Packet, raw []byte) bool {
	// The answer of a request that came over a link or a path travels back
	// the same way.
	if held := r.replyRoute(decoded); held != nil {
		if len(held.path) == 2 {
			return r.federation.forward(held.path[1], raw) == nil
		}
		return r.startRoute(decoded, raw, held.path, nil)
	}

	held := r.catalog.find(decoded.Dst)
	if held == nil {
		return false
	}

	// A partner that holds the destination itself takes the packet as it is.
	if len(held.path) == 1 {
		if err := r.federation.forward(held.peer, raw); err != nil {
			return false
		}
		r.rememberRoute(decoded, [][]byte{r.identity.PublicKey, held.peer})
		return true
	}

	path := append([][]byte{r.identity.PublicKey}, held.path...)
	return r.startRoute(decoded, raw, path, held.entry.Record)
}

// startRoute wraps a packet and gives it to the next relay of the path.
func (r *Relay) startRoute(decoded *packet.Packet, raw []byte, path [][]byte, record map[string]any) bool {
	mode := RouteReply
	if record != nil {
		mode = RouteRequest
	}

	wrapper, err := EncodeRoute(&Route{Mode: mode, Path: path, Cursor: 1, Packet: raw, Record: record})
	if err != nil {
		r.log.Debug("the route did not encode", "error", err)
		return false
	}

	if err := r.federation.forwardRoute(path[1], wrapper); err != nil {
		return false
	}
	if mode == RouteRequest {
		r.rememberRoute(decoded, path)
	}
	return true
}

// carryRoute takes one routed packet from a partner.
func (r *Relay) carryRoute(peer, raw []byte) {
	route, err := DecodeRoute(raw)
	if err != nil {
		r.drop(raw, "the route is not valid")
		return
	}

	// This relay must stand where the cursor says, and the packet must come
	// from the relay before it.
	if string(route.Path[route.Cursor]) != string(r.identity.PublicKey) {
		r.drop(raw, "the route names another relay here")
		return
	}
	if string(route.Path[route.Cursor-1]) != string(peer) {
		r.drop(raw, "the route came from the wrong relay")
		return
	}

	decoded, err := decodeForPeer(route.Packet)
	if err != nil {
		r.drop(raw, "the packet inside the route is not valid")
		return
	}

	if route.Cursor == len(route.Path)-1 {
		r.finishRoute(route, decoded)
		return
	}
	r.passRoute(route, decoded)
}

// finishRoute delivers a routed packet to a citizen of this relay.
func (r *Relay) finishRoute(route *Route, decoded *packet.Packet) {
	if route.Mode == RouteRequest {
		if !r.allowsRoutedRequest(route, decoded) {
			r.drop(route.Packet, "the destination does not take a routed request")
			return
		}

		// The answer travels the path backwards. A conversation is named
		// by the citizen that started it, then the one that answers.
		r.remember(decoded.Src, decoded.Dst, decoded.SessionID, reversed(route.Path))
	} else if !r.allowedReply(decoded, route.Path) {
		r.drop(route.Packet, "no conversation takes this reply")
		return
	}

	r.mu.RLock()
	destination := r.routes[string(decoded.Dst)]
	r.mu.RUnlock()

	if destination == nil {
		r.drop(route.Packet, "no route")
		return
	}
	if !destination.send(route.Packet) {
		r.drop(route.Packet, "the destination is behind")
	}
}

// passRoute hands a routed packet to the next relay. Only an operator that
// allows transit lets one through.
func (r *Relay) passRoute(route *Route, decoded *packet.Packet) {
	if !r.options.Transit {
		r.drop(route.Packet, "this relay does not pass traffic on")
		return
	}

	next := route.Path[route.Cursor+1]

	r.federation.mu.Lock()
	_, approved := r.federation.peers[string(next)]
	r.federation.mu.Unlock()

	if !approved {
		r.drop(route.Packet, "the next relay of the route is not a partner")
		return
	}

	if route.Mode == RouteRequest {
		if !r.validRecord(route, decoded) {
			r.drop(route.Packet, "the record of the route does not hold")
			return
		}
		r.remember(decoded.Src, decoded.Dst, decoded.SessionID, reversed(route.Path))
	} else if !r.allowedReply(decoded, route.Path) {
		r.drop(route.Packet, "no conversation takes this reply")
		return
	}

	onward := *route
	onward.Cursor = route.Cursor + 1

	wrapper, err := EncodeRoute(&onward)
	if err != nil {
		r.drop(route.Packet, "the route did not encode again")
		return
	}
	if err := r.federation.forwardRoute(next, wrapper); err != nil {
		r.drop(route.Packet, "the next relay did not take the route")
	}
}

// allowsRoutedRequest checks the signed permission of the provider, and its
// publication as it stands now. A provider that stops sharing refuses a new
// request, whatever an older record says.
func (r *Relay) allowsRoutedRequest(route *Route, decoded *packet.Packet) bool {
	if !r.validRecord(route, decoded) {
		return false
	}

	r.mu.Lock()
	held, there := r.directory[string(decoded.Dst)]
	r.mu.Unlock()

	return there && held.entry.Federation == announce.Network &&
		held.entry.Federatable(r.identity.PublicKey)
}

// validRecord checks that the announcement of a route names the destination,
// shares with the network, and calls the last relay of the path home.
func (r *Relay) validRecord(route *Route, decoded *packet.Packet) bool {
	if route.Record == nil {
		return false
	}

	entry, err := announce.Verify(route.Record, time.Now())
	if err != nil {
		return false
	}

	home := route.Path[len(route.Path)-1]
	return entry.Federation == announce.Network &&
		string(entry.PublicKey) == string(decoded.Dst) &&
		string(entry.RelayPublicKey) == string(home)
}

// remember lets one reply through, along the path that it must take.
func (r *Relay) remember(first, second, sessionID []byte, path [][]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.paths) >= MaxDirectoryRecords {
		return
	}
	r.paths[routeKey(first, second, sessionID)] = &conversation{
		path: path, expiresAt: time.Now().Add(ConversationLife),
	}
}

// rememberRoute lets the answer of a request that left this relay come back.
func (r *Relay) rememberRoute(decoded *packet.Packet, path [][]byte) {
	r.remember(decoded.Src, decoded.Dst, decoded.SessionID, path)
}

// replyRoute reads the way back for a reply, and nothing for anything else.
func (r *Relay) replyRoute(decoded *packet.Packet) *conversation {
	r.mu.Lock()
	defer r.mu.Unlock()

	held, there := r.paths[routeKey(decoded.Dst, decoded.Src, decoded.SessionID)]
	if !there || held.expiresAt.Before(time.Now()) || len(held.path) < 2 {
		return nil
	}
	// The path of the conversation runs from here to the other side.
	if string(held.path[0]) != string(r.identity.PublicKey) {
		return nil
	}
	return held
}

// allowedReply says whether a reply belongs to a conversation that this
// relay let through, and travels the path of that conversation. The relay
// that started the conversation holds the path the other way round, so
// either direction of the same route passes.
func (r *Relay) allowedReply(decoded *packet.Packet, path [][]byte) bool {
	r.mu.Lock()
	defer r.mu.Unlock()

	held, there := r.paths[routeKey(decoded.Dst, decoded.Src, decoded.SessionID)]
	if !there || held.expiresAt.Before(time.Now()) {
		return false
	}
	return samePath(held.path, path) || samePath(held.path, reversed(path))
}

func routeKey(first, second, sessionID []byte) string {
	return hex.EncodeToString(first) + hex.EncodeToString(second) + hex.EncodeToString(sessionID)
}

func reversed(path [][]byte) [][]byte {
	out := make([][]byte, 0, len(path))
	for index := len(path) - 1; index >= 0; index-- {
		out = append(out, path[index])
	}
	return out
}

func samePath(left, right [][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if string(left[index]) != string(right[index]) {
			return false
		}
	}
	return true
}
