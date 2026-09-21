// Package wire holds the framing and the handshake of a relay connection.
//
// Framing: a 4 byte big endian length, then that many bytes.
//
// Handshake, version 1:
//
//	relay  -> client: relay_public_key[32] challenge[32]   (no frame)
//	relay  -> client: frame("ARC_RELAY_INFO_V1" max_frame_bytes[4])
//	client -> relay:  client_public_key[32] signature[64]  (no frame)
//
// The signature proves that the client holds the secret key:
//
//	signature = Sign(secret, "ARC_RELAY_AUTH_V1" + relay_public_key + challenge + client_public_key)
//
// A max_frame_bytes of 0 means that the relay accepts a frame of any size.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/gezibash/arc/identity"
)

// The prefixes and the sizes of the relay protocol.
const (
	DirectoryPrefix  = "ARC_DIRECTORY_V1"
	FederationPrefix = "ARC_FEDERATION_V1"
	RelayInfoMagic   = "ARC_RELAY_INFO_V1"
	AuthContext      = "ARC_RELAY_AUTH_V1"

	ChallengeBytes   = 32
	SignatureBytes   = 64
	RelayHelloBytes  = identity.SeedBytes + ChallengeBytes
	ClientHelloBytes = identity.SeedBytes + SignatureBytes

	// MaxDirectoryControlBytes caps one directory control message.
	MaxDirectoryControlBytes = 256 * 1024
)

// ErrFrameTooLarge reports a frame over the cap of this side.
var ErrFrameTooLarge = errors.New("wire: frame too large")

// ErrInvalidHello reports a handshake that does not have the right shape.
var ErrInvalidHello = errors.New("wire: invalid hello")

// WriteFrame writes one length prefixed frame.
func WriteFrame(w io.Writer, payload []byte) error {
	header := binary.BigEndian.AppendUint32(make([]byte, 0, 4), uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one length prefixed frame. A max of zero accepts a frame of
// any size that fits in memory.
func ReadFrame(r io.Reader, max uint32) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}

	length := binary.BigEndian.Uint32(header[:])
	if max > 0 && length > max {
		return nil, fmt.Errorf("%w: %d bytes", ErrFrameTooLarge, length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

// ReadRelayHello reads the first 64 bytes of a relay connection.
func ReadRelayHello(r io.Reader) (relayPublicKey, challenge []byte, err error) {
	hello := make([]byte, RelayHelloBytes)
	if _, err := io.ReadFull(r, hello); err != nil {
		return nil, nil, err
	}
	return hello[:identity.SeedBytes], hello[identity.SeedBytes:], nil
}

// RelayHello builds the first 64 bytes that a relay writes.
func RelayHello(relayPublicKey, challenge []byte) ([]byte, error) {
	if len(relayPublicKey) != identity.SeedBytes || len(challenge) != ChallengeBytes {
		return nil, ErrInvalidHello
	}
	return append(append([]byte{}, relayPublicKey...), challenge...), nil
}

// ClientHello builds the 96 bytes that a client writes in answer.
func ClientHello(me *identity.Identity, relayPublicKey, challenge []byte) ([]byte, error) {
	if len(relayPublicKey) != identity.SeedBytes || len(challenge) != ChallengeBytes {
		return nil, ErrInvalidHello
	}

	out := make([]byte, 0, ClientHelloBytes)
	out = append(out, me.PublicKey...)
	return append(out, me.Sign(proof(relayPublicKey, challenge, me.PublicKey))...), nil
}

// VerifyClientHello checks the answer of a client.
func VerifyClientHello(hello, relayPublicKey, challenge []byte) ([]byte, bool) {
	if len(hello) != ClientHelloBytes || len(relayPublicKey) != identity.SeedBytes || len(challenge) != ChallengeBytes {
		return nil, false
	}

	publicKey := hello[:identity.SeedBytes]
	signature := hello[identity.SeedBytes:]
	if !identity.Verify(publicKey, proof(relayPublicKey, challenge, publicKey), signature) {
		return nil, false
	}
	return publicKey, true
}

// RelayInfo builds the control frame that advertises the frame cap. A cap of
// zero means that the relay accepts a frame of any size.
func RelayInfo(maxFrameBytes uint32) []byte {
	return binary.BigEndian.AppendUint32([]byte(RelayInfoMagic), maxFrameBytes)
}

// DecodeRelayInfo reads the frame cap out of a relay info frame.
func DecodeRelayInfo(payload []byte) (uint32, bool) {
	if len(payload) != len(RelayInfoMagic)+4 || string(payload[:len(RelayInfoMagic)]) != RelayInfoMagic {
		return 0, false
	}
	return binary.BigEndian.Uint32(payload[len(RelayInfoMagic):]), true
}

// IsDirectory says whether a frame carries a directory control message, and
// returns the JSON inside it.
func IsDirectory(payload []byte) ([]byte, bool) {
	if len(payload) < len(DirectoryPrefix) || string(payload[:len(DirectoryPrefix)]) != DirectoryPrefix {
		return nil, false
	}
	return payload[len(DirectoryPrefix):], true
}

// Directory wraps a directory control message in its prefix.
func Directory(payload []byte) []byte {
	return append([]byte(DirectoryPrefix), payload...)
}

func proof(relayPublicKey, challenge, publicKey []byte) []byte {
	out := make([]byte, 0, len(AuthContext)+len(relayPublicKey)+len(challenge)+len(publicKey))
	out = append(out, AuthContext...)
	out = append(out, relayPublicKey...)
	out = append(out, challenge...)
	return append(out, publicKey...)
}
