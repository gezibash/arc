// Package relayserver composes the ARC relay: a khatru relay with its
// event store, the sealed-data rule, the write limits, the connection log,
// and rings when the relay offers them.
package relayserver

import (
	"log/slog"
	"net/http"
	"slices"

	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/relaylog"
	"github.com/gezibash/arc/adapters/relay/rings"
	"github.com/gezibash/arc/adapters/relay/sealed"
	"github.com/gezibash/arc/adapters/relay/turn"
)

// New makes the relay of arc-relay, with its store in the file at
// path. The relay logs its connections to logger. If ring is not nil, the
// relay offers rings. Call the returned function to close the store.
func New(path string, p limits.Policy, ring *rings.Config, logger *slog.Logger) (*khatru.Relay, *relaylog.Log, *rings.Rings, func(), error) {
	db := &boltdb.BoltBackend{Path: path}
	if err := db.Init(); err != nil {
		return nil, nil, nil, nil, err
	}
	connections := relaylog.New(logger)
	p.Refused = connections.Refused
	rl := khatru.NewRelay()
	rl.UseEventstore(db, 500)
	rl.Negentropy = true
	// khatru copies Info for each NIP-11 request and appends to the copy;
	// with no spare room, each append copies, and requests do not race (#425).
	rl.Info.SupportedNIPs = slices.Clip(append(rl.Info.SupportedNIPs, 77))
	readers := sealed.Protect(rl)
	limits.Apply(rl, db.DB, p)
	connections.Watch(rl)
	var ringer *rings.Rings
	if ring != nil {
		config := *ring
		config.Readers = readers
		config.Logger = logger
		var err error
		if ringer, err = rings.Apply(rl, db.DB, config); err != nil {
			db.Close()
			return nil, nil, nil, nil, err
		}
	}
	return rl, connections, ringer, db.Close, nil
}

// Handler serves the relay, and the HTTP paths of TURN and rings when
// the relay offers them. Every other path is the relay's: khatru answers
// 404 on a path that it does not know.
func Handler(rl *khatru.Relay, turnServer *turn.Server, ringer *rings.Rings) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", rl)
	if turnServer != nil {
		mux.Handle(turn.Path, turnServer)
	}
	if ringer != nil {
		mux.Handle(rings.Path, ringer)
		mux.Handle(rings.TestPath, ringer)
	}
	return mux
}
