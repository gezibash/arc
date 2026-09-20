package relay

import (
	"encoding/binary"
	"encoding/json"
	"errors"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/canonical"
)

// A routed packet travels between relays that do not share a connection. The
// wrapper names the relays on the way, and the cursor says which one holds it
// now:
//
//	version:u8, mode:u8, cursor:u8, path_count:u8,
//	packet_bytes:u32be, record_bytes:u16be,
//	path:32*path_count bytes, inner_packet, announcement_json
//
// The inner packet stays as the citizen signed it. A relay moves the cursor
// and passes the wrapper on. The announcement travels with a request, so
// every relay on the way sees the signed permission of the provider.

// The limits of one routed packet.
const (
	routeVersion   = 1
	MaxRoutePacket = 8 * 1024 * 1024
	MaxRouteRecord = 8192
	MaxRoutePath   = 9
	MinRoutePath   = 2
)

// RouteMode says whether a wrapper carries a request or a reply.
type RouteMode uint8

// The two directions of a routed packet.
const (
	RouteRequest RouteMode = 1
	RouteReply   RouteMode = 2
)

// ErrInvalidRoute reports a wrapper that does not hold together.
var ErrInvalidRoute = errors.New("relay: invalid route")

// Route is one routed packet.
type Route struct {
	// Mode says whether this is a request or a reply.
	Mode RouteMode
	// Path names the relays from the first to the last, each once.
	Path [][]byte
	// Cursor is the place of the receiving relay in the path.
	Cursor int
	// Packet is the packet of the citizen, unchanged.
	Packet []byte
	// Record is the signed announcement of the provider. A request carries
	// one, and a reply carries none.
	Record map[string]any
}

// EncodeRoute writes one wrapper.
func EncodeRoute(route *Route) ([]byte, error) {
	if route == nil || (route.Mode != RouteRequest && route.Mode != RouteReply) {
		return nil, ErrInvalidRoute
	}
	if !validPath(route.Path) {
		return nil, ErrInvalidRoute
	}
	if route.Cursor < 1 || route.Cursor >= len(route.Path) {
		return nil, ErrInvalidRoute
	}
	if len(route.Packet) > MaxRoutePacket {
		return nil, ErrInvalidRoute
	}

	var record []byte
	if route.Mode == RouteRequest {
		if !signedRecord(route.Record) {
			return nil, ErrInvalidRoute
		}

		encoded, err := json.Marshal(route.Record)
		if err != nil || len(encoded) > MaxRouteRecord {
			return nil, ErrInvalidRoute
		}
		record = encoded
	} else if route.Record != nil {
		return nil, ErrInvalidRoute
	}

	out := make([]byte, 0, 10+len(route.Path)*identity.SeedBytes+len(route.Packet)+len(record))
	out = append(out, routeVersion, uint8(route.Mode), uint8(route.Cursor), uint8(len(route.Path)))
	out = binary.BigEndian.AppendUint32(out, uint32(len(route.Packet)))
	out = binary.BigEndian.AppendUint16(out, uint16(len(record)))

	for _, hop := range route.Path {
		out = append(out, hop...)
	}
	out = append(out, route.Packet...)
	return append(out, record...), nil
}

// DecodeRoute reads one wrapper.
func DecodeRoute(raw []byte) (*Route, error) {
	if len(raw) < 10 || raw[0] != routeVersion {
		return nil, ErrInvalidRoute
	}

	mode := RouteMode(raw[1])
	if mode != RouteRequest && mode != RouteReply {
		return nil, ErrInvalidRoute
	}

	cursor := int(raw[2])
	hops := int(raw[3])
	packetLen := int(binary.BigEndian.Uint32(raw[4:8]))
	recordLen := int(binary.BigEndian.Uint16(raw[8:10]))

	if hops < MinRoutePath || hops > MaxRoutePath || packetLen > MaxRoutePacket || recordLen > MaxRouteRecord {
		return nil, ErrInvalidRoute
	}
	if cursor < 1 || cursor >= hops {
		return nil, ErrInvalidRoute
	}

	rest := raw[10:]
	if len(rest) != hops*identity.SeedBytes+packetLen+recordLen {
		return nil, ErrInvalidRoute
	}

	path := make([][]byte, 0, hops)
	for index := 0; index < hops; index++ {
		at := index * identity.SeedBytes
		path = append(path, rest[at:at+identity.SeedBytes])
	}
	if !validPath(path) {
		return nil, ErrInvalidRoute
	}

	rest = rest[hops*identity.SeedBytes:]
	packet := rest[:packetLen]
	recordBytes := rest[packetLen:]

	route := &Route{Mode: mode, Path: path, Cursor: cursor, Packet: packet}

	switch {
	case mode == RouteReply && len(recordBytes) == 0:
	case mode == RouteRequest && len(recordBytes) > 0:
		// The signature of the record covers the digits that arrived, so
		// the numbers keep their form.
		value, err := canonical.Decode(recordBytes)
		if err != nil {
			return nil, ErrInvalidRoute
		}

		record, ok := value.(map[string]any)
		if !ok || !signedRecord(record) {
			return nil, ErrInvalidRoute
		}
		route.Record = record
	default:
		return nil, ErrInvalidRoute
	}
	return route, nil
}

// validPath says whether a path names two to nine relays, each once.
func validPath(path [][]byte) bool {
	if len(path) < MinRoutePath || len(path) > MaxRoutePath {
		return false
	}

	seen := map[string]bool{}
	for _, hop := range path {
		if len(hop) != identity.SeedBytes || seen[string(hop)] {
			return false
		}
		seen[string(hop)] = true
	}
	return true
}

// signedRecord says whether a record holds the fields of a signed
// announcement.
func signedRecord(record map[string]any) bool {
	if record == nil {
		return false
	}

	for _, field := range []string{
		"version", "public_key", "capabilities", "issued_at", "expires_at", "signature",
	} {
		if _, held := record[field]; !held {
			return false
		}
	}
	return true
}
