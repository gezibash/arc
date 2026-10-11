// Package files holds what the two ends of a transfer share: the messages
// of the ARC call, the link of an offer, and the part file of a file that
// arrives. The pipe itself is the package sdk/direct.
package files

import "github.com/pion/webrtc/v4"

// OpPut is the operation of a request that gives a file to the app.
const OpPut = "put"

// Fetch is the body of the ARC call to the app. With no operation, a
// receiver asks for an offered file. With the operation "put", a citizen
// gives a file to the app.
type Fetch struct {
	Version int `json:"v"`
	// Op is empty for a fetch, or OpPut.
	Op     string `json:"op,omitempty"`
	SHA256 string `json:"sha256"`
	// Offset is the number of bytes that a receiver has. A put leaves it 0.
	Offset int64 `json:"offset"`
	// Size is the size of the file of a put.
	Size int64 `json:"size,omitempty"`
	// HoldMS is the time that the app waits before it sends its first
	// packet, in milliseconds. The caller then sends first.
	HoldMS int `json:"hold_ms,omitempty"`
	// STUN is a STUN URL with an IPv4 address, such as
	// "stun:192.0.2.1:3478". A caller that uses the TURN server of a relay
	// names that server here. The app then asks it for its public address
	// too, because the TURN server lets in only packets from the addresses
	// in the answer.
	STUN string                    `json:"stun,omitempty"`
	SDP  webrtc.SessionDescription `json:"sdp"`
}

// Answer is the reply of the app.
type Answer struct {
	Version int `json:"v"`
	// Offset is the number of bytes of a put that the app has. If it is
	// the size of the file, the answer has no description, and no bytes go.
	Offset int64                     `json:"offset,omitempty"`
	SDP    webrtc.SessionDescription `json:"sdp,omitzero"`
}
