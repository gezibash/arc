package relay_test

import (
	"bytes"
	"testing"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/relay"
)

func keys(t *testing.T, count int) [][]byte {
	t.Helper()

	out := make([][]byte, 0, count)
	for index := 0; index < count; index++ {
		me, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, me.PublicKey)
	}
	return out
}

func record() map[string]any {
	return map[string]any{
		"version": 3, "public_key": "ab", "capabilities": []any{},
		"issued_at": 1, "expires_at": 2, "signature": "cd",
		"federation": "network", "relay_public_key": "ef",
	}
}

func TestRouteRoundTrip(t *testing.T) {
	path := keys(t, 3)
	packet := []byte("the packet of a citizen")

	raw, err := relay.EncodeRoute(&relay.Route{
		Mode: relay.RouteRequest, Path: path, Cursor: 1, Packet: packet, Record: record(),
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := relay.DecodeRoute(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Mode != relay.RouteRequest || got.Cursor != 1 || !bytes.Equal(got.Packet, packet) {
		t.Errorf("route = %+v", got)
	}
	if len(got.Path) != 3 || !bytes.Equal(got.Path[2], path[2]) {
		t.Errorf("path = %v", got.Path)
	}
	if got.Record["public_key"] != "ab" {
		t.Errorf("record = %v", got.Record)
	}

	// A reply carries no record.
	raw, err = relay.EncodeRoute(&relay.Route{
		Mode: relay.RouteReply, Path: path, Cursor: 2, Packet: packet,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err = relay.DecodeRoute(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != relay.RouteReply || got.Record != nil {
		t.Errorf("reply = %+v", got)
	}
}

func TestRouteRefusesWhatDoesNotHold(t *testing.T) {
	path := keys(t, 3)
	packet := []byte("x")

	cases := map[string]*relay.Route{
		"no mode":                    {Path: path, Cursor: 1, Packet: packet, Record: record()},
		"one relay":                  {Mode: relay.RouteRequest, Path: path[:1], Cursor: 1, Packet: packet, Record: record()},
		"a cursor at the front":      {Mode: relay.RouteRequest, Path: path, Cursor: 0, Packet: packet, Record: record()},
		"a cursor past the end":      {Mode: relay.RouteRequest, Path: path, Cursor: 3, Packet: packet, Record: record()},
		"a request without a record": {Mode: relay.RouteRequest, Path: path, Cursor: 1, Packet: packet},
		"a reply with a record":      {Mode: relay.RouteReply, Path: path, Cursor: 1, Packet: packet, Record: record()},
		"a relay twice":              {Mode: relay.RouteRequest, Path: [][]byte{path[0], path[0]}, Cursor: 1, Packet: packet, Record: record()},
	}

	for name, route := range cases {
		if _, err := relay.EncodeRoute(route); err == nil {
			t.Errorf("%s: the route encoded", name)
		}
	}

	raw, err := relay.EncodeRoute(&relay.Route{
		Mode: relay.RouteRequest, Path: path, Cursor: 1, Packet: packet, Record: record(),
	})
	if err != nil {
		t.Fatal(err)
	}

	for index := range raw[:10] {
		changed := append([]byte(nil), raw...)
		changed[index] ^= 0xFF

		if _, err := relay.DecodeRoute(changed); err == nil {
			t.Errorf("a header with byte %d changed still decoded", index)
		}
	}

	for name, value := range map[string][]byte{
		"empty":          nil,
		"a short header": raw[:8],
		"a cut body":     raw[:len(raw)-1],
	} {
		if _, err := relay.DecodeRoute(value); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

// A path of nine relays is the most that a route carries.
func TestRouteHoldsItsLength(t *testing.T) {
	packet := []byte("x")

	if _, err := relay.EncodeRoute(&relay.Route{
		Mode: relay.RouteRequest, Path: keys(t, relay.MaxRoutePath), Cursor: 1,
		Packet: packet, Record: record(),
	}); err != nil {
		t.Errorf("a path of %d relays was refused: %v", relay.MaxRoutePath, err)
	}

	if _, err := relay.EncodeRoute(&relay.Route{
		Mode: relay.RouteRequest, Path: keys(t, relay.MaxRoutePath+1), Cursor: 1,
		Packet: packet, Record: record(),
	}); err == nil {
		t.Error("a path over the limit passed")
	}
}
