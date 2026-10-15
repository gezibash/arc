//go:build !arctest

package private

// Work is the NIP-13 difficulty, in bits, that admits a wrap without a
// stamp: from a stranger, on the recipient's device, and on an inbox relay
// that asks for it. It is a protocol constant, so each sender knows it.
// docs/strangers/SPEC.md, section 3.5.
const Work = 16
