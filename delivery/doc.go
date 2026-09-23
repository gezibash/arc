// Package delivery holds the ARC stack: a signed Nostr event is the unit,
// and a node moves events over any transport that can carry them.
//
// The design is docs/delivery/SPEC.md. The packages below it hold the
// identity, the store, the node, the relay and file transports, private
// events, mail, calls, and the capability catalog.
package delivery
