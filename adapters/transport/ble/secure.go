package ble

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"fiatjaf.com/nostr"
	"github.com/flynn/noise"
	"github.com/gezibash/arc/core/store"
)

// packetLink is the GATT I/O boundary, not an ARC event transport. Writes and
// reads preserve packet order. Close must unblock both directions.
type packetLink interface {
	write(context.Context, []byte) error
	read(context.Context) ([]byte, error)
	Close() error
}

const maxMessage = 256 << 10
const recordBytes = 4096
const linkDomain = "arc-ble-direct-v1"

type secureLink struct {
	link   packetLink
	tx, rx *noise.CipherState
	sendMu sync.Mutex
}

func (s *secureLink) Close() error { return s.link.Close() }
func writeRecord(ctx context.Context, l packetLink, b []byte) error {
	if len(b) == 0 || len(b) > recordBytes {
		return errors.New("bluetooth: invalid record length")
	}
	wire := make([]byte, 2+len(b))
	binary.BigEndian.PutUint16(wire, uint16(len(b)))
	copy(wire[2:], b)
	for len(wire) > 0 {
		n := min(20, len(wire))
		if err := l.write(ctx, wire[:n]); err != nil {
			return err
		}
		wire = wire[n:]
	}
	return nil
}
func readRecord(ctx context.Context, l packetLink) ([]byte, error) {
	first, err := l.read(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	wire := make([]byte, 0, recordBytes+2)
	packet := first
	size := 0
	for {
		if len(packet) == 0 || len(packet) > 20 {
			return nil, errors.New("bluetooth: invalid GATT packet")
		}
		wire = append(wire, packet...)
		if size == 0 && len(wire) >= 2 {
			size = int(binary.BigEndian.Uint16(wire)) + 2
			if size <= 2 || size > recordBytes+2 {
				return nil, errors.New("bluetooth: oversized record")
			}
		}
		if size > 0 && len(wire) >= size {
			if len(wire) != size {
				return nil, errors.New("bluetooth: record boundary mismatch")
			}
			return wire[2:], nil
		}
		packet, err = l.read(ctx)
		if err != nil {
			return nil, err
		}
	}
}
func (s *secureLink) send(ctx context.Context, b []byte) error {
	if len(b) == 0 || len(b) > maxMessage {
		return errors.New("bluetooth: message size limit")
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	wire := make([]byte, 4+len(b))
	binary.BigEndian.PutUint32(wire, uint32(len(b)))
	copy(wire[4:], b)
	for len(wire) > 0 {
		n := min(recordBytes-16, len(wire))
		cipher, err := s.tx.Encrypt(nil, nil, wire[:n])
		if err != nil {
			return err
		}
		if err = writeRecord(ctx, s.link, cipher); err != nil {
			return err
		}
		wire = wire[n:]
	}
	return nil
}
func (s *secureLink) receive(ctx context.Context) ([]byte, error) {
	// Idle watches can wait indefinitely; an in-progress message cannot.
	b, err := readRecord(ctx, s.link)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	plain, err := s.rx.Decrypt(nil, nil, b)
	if err != nil {
		return nil, err
	}
	if len(plain) < 4 {
		return nil, errors.New("bluetooth: short message")
	}
	n := int(binary.BigEndian.Uint32(plain))
	if n < 1 || n > maxMessage {
		return nil, errors.New("bluetooth: message size limit")
	}
	out := append([]byte(nil), plain[4:]...)
	for len(out) < n {
		b, err = readRecord(ctx, s.link)
		if err != nil {
			return nil, err
		}
		plain, err = s.rx.Decrypt(nil, nil, b)
		if err != nil {
			return nil, err
		}
		out = append(out, plain...)
	}
	if len(out) != n {
		return nil, errors.New("bluetooth: message boundary mismatch")
	}
	return out, nil
}
func authenticate(ctx context.Context, l packetLink, c DirectConfig, initiator bool) (*secureLink, error) {
	if c.Signer == nil || c.Store == nil || c.Share == nil || c.Peer == (nostr.PubKey{}) {
		return nil, errors.New("bluetooth: signer, expected peer, store and share policy are required")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	suite := noise.NewCipherSuite(noise.DH25519, noise.CipherChaChaPoly, noise.HashSHA256)
	key, err := suite.GenerateKeypair(rand.Reader)
	if err != nil {
		return nil, err
	}
	hs, err := noise.NewHandshakeState(noise.Config{CipherSuite: suite, Pattern: noise.HandshakeXX, Initiator: initiator, Prologue: []byte(linkDomain), StaticKeypair: key})
	if err != nil {
		return nil, err
	}
	var tx, rx *noise.CipherState
	for i := range 3 {
		if (i%2 == 0) == initiator {
			b, a, z, e := hs.WriteMessage(nil, nil)
			if e != nil {
				return nil, e
			}
			if e = writeRecord(ctx, l, b); e != nil {
				return nil, e
			}
			if a != nil {
				tx, rx = a, z
			}
		} else {
			b, e := readRecord(ctx, l)
			if e != nil {
				return nil, e
			}
			payload, a, z, e := hs.ReadMessage(nil, b)
			if e != nil {
				return nil, e
			}
			if len(payload) != 0 {
				return nil, errors.New("bluetooth: unexpected handshake payload")
			}
			if a != nil {
				tx, rx = a, z
			}
		}
	}
	if !initiator {
		tx, rx = rx, tx
	}
	s := &secureLink{link: l, tx: tx, rx: rx}
	// An ordinary signed event is a connection-local proof, never published or
	// saved. Its exact content binds the ARC identity to the Noise transcript,
	// role, and intended peer. Signing remains with the existing ARC signer.
	role, peerRole := "initiator", "responder"
	if !initiator {
		role, peerRole = peerRole, role
	}
	binding := hex.EncodeToString(hs.ChannelBinding())
	proof := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Tags: nostr.Tags{}, Content: linkDomain + ":" + binding + ":" + role + ":" + c.Peer.Hex()}
	if err = c.Signer.SignEvent(ctx, &proof); err != nil {
		return nil, err
	}
	b, err := json.Marshal(proof)
	if err != nil {
		return nil, err
	}
	var incoming []byte
	// Alternating proofs avoids deadlock on a small GATT receive queue.
	if initiator {
		err = s.send(ctx, b)
		if err == nil {
			incoming, err = s.receive(ctx)
		}
	} else {
		incoming, err = s.receive(ctx)
	}
	if err != nil {
		return nil, err
	}
	var other nostr.Event
	if err = json.Unmarshal(incoming, &other); err != nil {
		return nil, err
	}
	if err = store.Verify(other); err != nil {
		return nil, err
	}
	want := linkDomain + ":" + binding + ":" + peerRole + ":" + c.Signer.PublicKey().Hex()
	if other.Kind != 1 || len(other.Tags) != 0 || other.PubKey != c.Peer || other.Content != want {
		return nil, errors.New("bluetooth: peer identity or channel binding mismatch")
	}
	if !initiator {
		if err = s.send(ctx, b); err != nil {
			return nil, err
		}
	}
	return s, nil
}

var _ io.Closer = (*secureLink)(nil)
