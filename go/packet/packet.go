// Package packet holds the ARC wire packet.
//
//	[4 bytes]   header length, big endian uint32
//	[N bytes]   header, JSON: {src, dst, sid, seq, ts, ph, ek?}
//	[64 bytes]  Ed25519 signature over the header bytes
//	[N bytes]   payload: nonce, 12 bytes, then the ChaCha20-Poly1305 ciphertext
//
// Fields:
//
//	src  base64 of the Ed25519 public key of the sender
//	dst  base64 of the Ed25519 public key of the recipient
//	sid  base64 of the session id, 16 bytes from the session setup
//	seq  the sequence number of the session, which guards against replay
//	ts   unix milliseconds, which gives freshness
//	ph   base64 of SHA-256 of the payload, which binds the payload to the header
//	ek   base64 of the ephemeral X25519 public key of the initiator. Every
//	     session version 2 packet carries it. A version 1 packet does not.
//
// The signature covers the header bytes. A valid signature proves that the
// sender holds the secret key of src. The ph field stops an attacker from
// replacing the payload under a signed header.
//
// The hash is SHA-256 until a BLAKE3 change to the wire format.
package packet

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gezibash/arc/go/identity"
)

const (
	lengthBytes    = 4
	signatureBytes = 64
	// NonceBytes is the length of the nonce at the front of the payload.
	NonceBytes = 12
	// MaxHeaderBytes caps the header of a packet from an unknown peer.
	MaxHeaderBytes = 64 * 1024
)

// Errors of this package. A peer that is not trusted sends the bytes, so every
// failure returns one of these and never a panic.
var (
	ErrMalformed           = errors.New("packet: malformed packet")
	ErrInvalidSignature    = errors.New("packet: invalid signature")
	ErrPayloadHashMismatch = errors.New("packet: payload hash mismatch")
)

// Packet holds the fields of one decoded packet.
type Packet struct {
	Src             []byte
	Dst             []byte
	SessionID       []byte
	Seq             uint64
	Timestamp       int64
	EphemeralPublic []byte
	Nonce           []byte
	Ciphertext      []byte
}

// Option changes one field of an encoded packet.
type Option func(*options)

type options struct {
	timestamp       int64
	ephemeralPublic []byte
}

// WithTimestamp sets the timestamp in unix milliseconds. Without it the packet
// carries the time of the call.
func WithTimestamp(unixMilli int64) Option {
	return func(o *options) { o.timestamp = unixMilli }
}

// WithEphemeralKey adds the ephemeral X25519 public key of the session. Every
// session version 2 packet carries it.
func WithEphemeralKey(key []byte) Option {
	return func(o *options) { o.ephemeralPublic = key }
}

type header struct {
	Src string `json:"src"`
	Dst string `json:"dst"`
	SID string `json:"sid"`
	Seq uint64 `json:"seq"`
	TS  int64  `json:"ts"`
	PH  string `json:"ph"`
	EK  string `json:"ek,omitempty"`
}

// Encode builds one signed packet.
func Encode(me *identity.Identity, dst, sessionID []byte, seq uint64, nonce, ciphertext []byte, opts ...Option) ([]byte, error) {
	config := options{timestamp: time.Now().UnixMilli()}
	for _, opt := range opts {
		opt(&config)
	}

	payload := make([]byte, 0, len(nonce)+len(ciphertext))
	payload = append(payload, nonce...)
	payload = append(payload, ciphertext...)
	sum := sha256.Sum256(payload)

	head := header{
		Src: encode64(me.PublicKey),
		Dst: encode64(dst),
		SID: encode64(sessionID),
		Seq: seq,
		TS:  config.timestamp,
		PH:  encode64(sum[:]),
	}
	if len(config.ephemeralPublic) == identity.SeedBytes {
		head.EK = encode64(config.ephemeralPublic)
	}

	headerBytes, err := json.Marshal(head)
	if err != nil {
		return nil, err
	}

	out := make([]byte, lengthBytes, lengthBytes+len(headerBytes)+signatureBytes+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(headerBytes)))
	out = append(out, headerBytes...)
	out = append(out, me.Sign(headerBytes)...)
	return append(out, payload...), nil
}

// Decode reads one packet, checks the signature over the header, and checks
// the payload against the hash in the header.
func Decode(raw []byte) (*Packet, error) {
	if len(raw) < lengthBytes {
		return nil, ErrMalformed
	}

	headerLen := int(binary.BigEndian.Uint32(raw[:lengthBytes]))
	if headerLen > MaxHeaderBytes || len(raw) < lengthBytes+headerLen+signatureBytes {
		return nil, ErrMalformed
	}

	headerBytes := raw[lengthBytes : lengthBytes+headerLen]
	signature := raw[lengthBytes+headerLen : lengthBytes+headerLen+signatureBytes]
	payload := raw[lengthBytes+headerLen+signatureBytes:]

	head, err := decodeHeader(headerBytes)
	if err != nil {
		return nil, err
	}

	src, err := decode64(head.Src, identity.SeedBytes)
	if err != nil {
		return nil, err
	}
	if !identity.Verify(src, headerBytes, signature) {
		return nil, ErrInvalidSignature
	}

	sum := sha256.Sum256(payload)
	if head.PH != encode64(sum[:]) {
		return nil, ErrPayloadHashMismatch
	}

	dst, err := decode64(head.Dst, 0)
	if err != nil {
		return nil, err
	}

	sessionID, err := decode64(head.SID, 0)
	if err != nil {
		return nil, err
	}

	var ephemeral []byte
	if head.EK != "" {
		if ephemeral, err = decode64(head.EK, identity.SeedBytes); err != nil {
			return nil, err
		}
	}

	if len(payload) < NonceBytes {
		return nil, ErrMalformed
	}

	return &Packet{
		Src:             src,
		Dst:             dst,
		SessionID:       sessionID,
		Seq:             head.Seq,
		Timestamp:       head.TS,
		EphemeralPublic: ephemeral,
		Nonce:           payload[:NonceBytes],
		Ciphertext:      payload[NonceBytes:],
	}, nil
}

// decodeHeader refuses anything that the Elixir implementation refuses: a
// header that is not an object, a sequence number that is not a whole number
// at or above zero, and a timestamp that is not a whole number.
func decodeHeader(raw []byte) (*header, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&fields); err != nil {
		return nil, ErrMalformed
	}
	if decoder.More() {
		return nil, ErrMalformed
	}

	head := &header{}
	var err error
	if head.Seq, err = wholeNumber(fields["seq"]); err != nil {
		return nil, err
	}
	if head.TS, err = signedNumber(fields["ts"]); err != nil {
		return nil, err
	}

	for value, field := range map[*string]string{
		&head.Src: "src", &head.Dst: "dst", &head.SID: "sid", &head.PH: "ph",
	} {
		if err := json.Unmarshal(fields[field], value); err != nil {
			return nil, ErrMalformed
		}
	}
	if raw, ok := fields["ek"]; ok {
		if err := json.Unmarshal(raw, &head.EK); err != nil {
			return nil, ErrMalformed
		}
	}
	return head, nil
}

func wholeNumber(raw json.RawMessage) (uint64, error) {
	value, err := strconv.ParseUint(string(raw), 10, 64)
	if err != nil {
		return 0, ErrMalformed
	}
	return value, nil
}

// signedNumber accepts a negative timestamp, because the Elixir implementation
// accepts one. The freshness check refuses it later.
func signedNumber(raw json.RawMessage) (int64, error) {
	value, err := strconv.ParseInt(string(raw), 10, 64)
	if err != nil {
		return 0, ErrMalformed
	}
	return value, nil
}

func encode64(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}

// decode64 decodes one base64 field. A want of zero accepts any length.
func decode64(value string, want int) ([]byte, error) {
	out, err := base64.StdEncoding.DecodeString(value)
	if err != nil || (want > 0 && len(out) != want) {
		return nil, ErrMalformed
	}
	return out, nil
}
