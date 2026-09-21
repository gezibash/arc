// Package delivery holds the new ARC stack: a signed Nostr event is the unit,
// and a node moves events over any transport that can carry them.
//
// The design is docs/delivery/SPEC.md. This is phase 1: the identity, the
// store, the node, and the relay and file transports. The older stack in
// identity, relay, client and citizen keeps working beside it until the new
// stack covers every command.
package delivery
