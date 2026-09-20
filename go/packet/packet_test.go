package packet_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/vectors"
	"github.com/gezibash/arc/go/packet"
	"github.com/gezibash/arc/go/session"
)

func decode(t *testing.T, value string) []byte {
	t.Helper()
	out, err := hex.DecodeString(value)
	if err != nil {
		t.Fatalf("decode %q: %v", value, err)
	}
	return out
}

// The Elixir implementation encoded this packet. Go reads every field, joins
// the session, and reads the message inside.
func TestReadsAnElixirPacket(t *testing.T) {
	want := vectors.Load(t).Session

	initiator, err := identity.FromSeedHex(want.InitiatorSeed)
	if err != nil {
		t.Fatal(err)
	}
	responder, err := identity.FromSeedHex(want.ResponderSeed)
	if err != nil {
		t.Fatal(err)
	}

	got, err := packet.Decode(decode(t, want.Packet))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}

	if !bytes.Equal(got.Src, initiator.PublicKey) {
		t.Error("src is not the initiator")
	}
	if !bytes.Equal(got.Dst, responder.PublicKey) {
		t.Error("dst is not the responder")
	}
	if h := hex.EncodeToString(got.SessionID); h != want.SessionID {
		t.Errorf("session id = %s, want %s", h, want.SessionID)
	}
	if h := hex.EncodeToString(got.EphemeralPublic); h != want.EphemeralPublic {
		t.Errorf("ephemeral key = %s, want %s", h, want.EphemeralPublic)
	}
	if got.Seq != want.Seq {
		t.Errorf("seq = %d, want %d", got.Seq, want.Seq)
	}
	if got.Timestamp != want.TS {
		t.Errorf("ts = %d, want %d", got.Timestamp, want.TS)
	}

	joined, err := session.Accept(responder, got.Src, got.EphemeralPublic, got.SessionID)
	if err != nil {
		t.Fatal(err)
	}

	plaintext, err := joined.Decrypt(got.Nonce, got.Ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(plaintext) != want.Plaintext {
		t.Errorf("plaintext = %q, want %q", plaintext, want.Plaintext)
	}
}

// The field order of the header differs between the two implementations. The
// signature covers the bytes that arrive, so the order does not matter.
func TestReadsAHeaderInAnotherFieldOrder(t *testing.T) {
	raw := decode(t, vectors.Load(t).Session.Packet)

	headerLen := int(binary.BigEndian.Uint32(raw[:4]))
	var fields map[string]any
	if err := json.Unmarshal(raw[4:4+headerLen], &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["ek"]; !ok {
		t.Fatal("the vector packet carries no ephemeral key")
	}
}

func roundTrip(t *testing.T, body []byte, opts ...packet.Option) (*identity.Identity, *packet.Packet, []byte) {
	t.Helper()

	alice, _ := identity.Generate()
	bob, _ := identity.Generate()
	sending, err := session.Establish(alice, bob.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	nonce, ciphertext, seq, err := sending.Encrypt(body)
	if err != nil {
		t.Fatal(err)
	}

	opts = append(opts, packet.WithEphemeralKey(sending.EphemeralPublic))
	raw, err := packet.Encode(alice, bob.PublicKey, sending.ID, seq, nonce, ciphertext, opts...)
	if err != nil {
		t.Fatal(err)
	}

	got, err := packet.Decode(raw)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return alice, got, raw
}

func TestRoundTrip(t *testing.T) {
	alice, got, _ := roundTrip(t, []byte("hello"), packet.WithTimestamp(1735689600000))

	if !bytes.Equal(got.Src, alice.PublicKey) {
		t.Error("src is not the sender")
	}
	if got.Timestamp != 1735689600000 {
		t.Errorf("ts = %d, want 1735689600000", got.Timestamp)
	}
	if len(got.Nonce) != packet.NonceBytes {
		t.Errorf("nonce is %d bytes, want %d", len(got.Nonce), packet.NonceBytes)
	}
}

func TestATimestampOfNowWhenNoneIsGiven(t *testing.T) {
	_, got, _ := roundTrip(t, []byte("hello"))

	if got.Timestamp < 1735689600000 {
		t.Errorf("ts = %d, which is before 2025", got.Timestamp)
	}
}

func TestRefusesAChangedPacket(t *testing.T) {
	_, _, raw := roundTrip(t, []byte("hello"))

	for index := range raw {
		changed := append([]byte(nil), raw...)
		changed[index] ^= 0x01
		if _, err := packet.Decode(changed); err == nil {
			t.Fatalf("a packet with byte %d changed still decoded", index)
		}
	}
}

func TestRefusesAChangedPayloadUnderASignedHeader(t *testing.T) {
	_, _, raw := roundTrip(t, []byte("hello"))

	headerLen := int(binary.BigEndian.Uint32(raw[:4]))
	payloadAt := 4 + headerLen + 64

	longer := append(append([]byte(nil), raw...), 'x')
	if _, err := packet.Decode(longer); err != packet.ErrPayloadHashMismatch {
		t.Errorf("a longer payload gave %v, want the hash error", err)
	}

	shorter := append([]byte(nil), raw[:len(raw)-1]...)
	if _, err := packet.Decode(shorter); err != packet.ErrPayloadHashMismatch {
		t.Errorf("a shorter payload gave %v, want the hash error", err)
	}

	if payloadAt >= len(raw) {
		t.Fatal("the packet carries no payload")
	}
}

func TestRefusesAnotherSigner(t *testing.T) {
	_, _, raw := roundTrip(t, []byte("hello"))

	mallory, _ := identity.Generate()
	headerLen := int(binary.BigEndian.Uint32(raw[:4]))
	signature := mallory.Sign(raw[4 : 4+headerLen])

	forged := append([]byte(nil), raw...)
	copy(forged[4+headerLen:], signature)

	if _, err := packet.Decode(forged); err != packet.ErrInvalidSignature {
		t.Errorf("a packet signed by another key gave %v, want the signature error", err)
	}
}

func TestRefusesAMalformedPacket(t *testing.T) {
	_, _, raw := roundTrip(t, []byte("hello"))

	tooLong := append([]byte(nil), raw...)
	binary.BigEndian.PutUint32(tooLong, 0xFFFFFFFF)

	cases := map[string][]byte{
		"empty":                           nil,
		"length alone":                    raw[:4],
		"a header cut off":                raw[:8],
		"a header longer than the packet": tooLong,
	}

	for name, value := range cases {
		if _, err := packet.Decode(value); err != packet.ErrMalformed {
			t.Errorf("%s: gave %v, want the malformed error", name, err)
		}
	}
}

// A peer that is not trusted writes the header. Every field of the wrong type
// is an error, and never a panic.
func TestRefusesAHeaderOfTheWrongShape(t *testing.T) {
	alice, _ := identity.Generate()

	headers := map[string]string{
		"a sequence that is a string":    `{"src":"%s","dst":"","sid":"","seq":"0","ts":0,"ph":"%s"}`,
		"a sequence with a fraction":     `{"src":"%s","dst":"","sid":"","seq":1.5,"ts":0,"ph":"%s"}`,
		"a sequence below zero":          `{"src":"%s","dst":"","sid":"","seq":-1,"ts":0,"ph":"%s"}`,
		"no sequence":                    `{"src":"%s","dst":"","sid":"","ts":0,"ph":"%s"}`,
		"a timestamp that is a string":   `{"src":"%s","dst":"","sid":"","seq":0,"ts":"0","ph":"%s"}`,
		"a source that is not base64":    `{"src":"%s","dst":"!!","sid":"","seq":0,"ts":0,"ph":"%s"}`,
		"an ephemeral key of 31 bytes":   `{"src":"%s","dst":"","sid":"","seq":0,"ts":0,"ph":"%s","ek":"AAAA"}`,
		"a header that is not an object": `["%s","%s"]`,
	}

	payload := make([]byte, packet.NonceBytes+16)
	for name, template := range headers {
		header := buildHeader(t, template, alice, payload)
		raw := frame(alice, header, payload)

		if _, err := packet.Decode(raw); err != packet.ErrMalformed {
			t.Errorf("%s: gave %v, want the malformed error", name, err)
		}
	}
}

func TestAPayloadShorterThanTheNonce(t *testing.T) {
	alice, _ := identity.Generate()
	payload := []byte{1, 2, 3}

	header := buildHeader(t, `{"src":"%s","dst":"","sid":"","seq":0,"ts":0,"ph":"%s"}`, alice, payload)
	if _, err := packet.Decode(frame(alice, header, payload)); err != packet.ErrMalformed {
		t.Error("a payload shorter than the nonce decoded")
	}
}

func buildHeader(t *testing.T, template string, me *identity.Identity, payload []byte) []byte {
	t.Helper()

	sum := sha256.Sum256(payload)
	src := base64.StdEncoding.EncodeToString(me.PublicKey)
	return []byte(fmt.Sprintf(template, src, base64.StdEncoding.EncodeToString(sum[:])))
}

func frame(me *identity.Identity, header, payload []byte) []byte {
	out := make([]byte, 4)
	binary.BigEndian.PutUint32(out, uint32(len(header)))
	out = append(out, header...)
	out = append(out, me.Sign(header)...)
	return append(out, payload...)
}
