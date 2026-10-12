package devices

import (
	"context"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
)

// RootsKind is the replaceable event that holds the wrapped data roots.
const RootsKind nostr.Kind = 10275

// MaxGenerations bounds the chain, so that the event stays below the event
// size limit with MaxDevices wraps.
const MaxGenerations = 128

// Root is the data root of one generation. Generation 0 is the keyed root,
// which follows from the citizen key. Each later generation is random.
type Root struct {
	Gen uint32
	Key [32]byte
}

// Roots is the opened roots event. It holds the newest generation, wrapped
// to the citizen key and to each device key, and the older random roots,
// sealed under the newest one. At is the time of the event.
type Roots struct {
	Gen     uint32
	Citizen string
	Devices map[nostr.PubKey]string
	Chain   string
	At      nostr.Timestamp
}

type rootsBody struct {
	Gen     uint32            `json:"gen"`
	Citizen string            `json:"citizen"`
	Devices map[string]string `json:"devices"`
	Chain   string            `json:"chain,omitempty"`
}

// ContentKey is the key of one app under a data root, as base.md 5.2 gives:
// HKDF-SHA256 of the root with the info "arc-data-v1", a zero byte and the
// app id.
func ContentKey(root [32]byte, app string) ([32]byte, error) {
	return derive(root, "arc-data-v1\x00"+app)
}

func derive(root [32]byte, info string) ([32]byte, error) {
	var key [32]byte
	b, err := hkdf.Key(sha256.New, root[:], nil, info, 32)
	copy(key[:], b)
	return key, err
}

// chainKey seals the older roots under a newer root.
func chainKey(root [32]byte) ([32]byte, error) { return derive(root, "arc-data-chain-v1") }

// NewGeneration makes a random root one generation after the newest root
// that the citizen holds, and wraps it to the citizen key and to each
// device. older holds each random root before it. k is the citizen key.
func NewGeneration(ctx context.Context, k nostr.Keyer, older map[uint32][32]byte, devs []nostr.PubKey, at nostr.Timestamp) (Roots, Root, error) {
	var next Root
	for gen := range older {
		next.Gen = max(next.Gen, gen)
	}
	next.Gen++
	if next.Gen > MaxGenerations {
		return Roots{}, Root{}, fmt.Errorf("devices: the citizen has %d generations of data roots, the most that ARC keeps", MaxGenerations)
	}
	if _, err := rand.Read(next.Key[:]); err != nil {
		return Roots{}, Root{}, err
	}
	r := Roots{Gen: next.Gen, At: at}
	if len(older) > 0 {
		plain := map[string]string{}
		for gen, key := range older {
			if gen == 0 {
				continue
			}
			plain[strconv.FormatUint(uint64(gen), 10)] = hex.EncodeToString(key[:])
		}
		text, err := json.Marshal(plain)
		if err != nil {
			return Roots{}, Root{}, err
		}
		ck, err := chainKey(next.Key)
		if err != nil {
			return Roots{}, Root{}, err
		}
		if r.Chain, err = nip44.Encrypt(string(text), ck); err != nil {
			return Roots{}, Root{}, err
		}
	}
	r, err := Grant(ctx, k, r, next, devs)
	return r, next, err
}

// Grant wraps the newest root to the citizen key and to each device key in
// devs, and drops the wraps of devices that are not in devs. It does not make
// a new generation, so a device that it drops still holds the root. Revoke
// with NewGeneration.
func Grant(ctx context.Context, k nostr.Keyer, r Roots, newest Root, devs []nostr.PubKey) (Roots, error) {
	if newest.Gen != r.Gen {
		return Roots{}, fmt.Errorf("devices: root of generation %d does not match generation %d", newest.Gen, r.Gen)
	}
	if len(devs) > MaxDevices {
		return Roots{}, fmt.Errorf("devices: more than %d devices", MaxDevices)
	}
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return Roots{}, err
	}
	plain := hex.EncodeToString(newest.Key[:])
	if r.Citizen, err = k.Encrypt(ctx, plain, me); err != nil {
		return Roots{}, err
	}
	old := r.Devices
	r.Devices = map[nostr.PubKey]string{}
	for _, d := range devs {
		if wrap, ok := old[d]; ok {
			r.Devices[d] = wrap
			continue
		}
		if r.Devices[d], err = k.Encrypt(ctx, plain, d); err != nil {
			return Roots{}, err
		}
	}
	return r, nil
}

// SealRoots makes the roots event. The citizen key k encrypts the content to
// itself with NIP-44 and signs the event, later than the event before it.
func SealRoots(ctx context.Context, k nostr.Keyer, r Roots, now nostr.Timestamp) (nostr.Event, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return nostr.Event{}, err
	}
	b := rootsBody{Gen: r.Gen, Citizen: r.Citizen, Chain: r.Chain, Devices: map[string]string{}}
	for d, wrap := range r.Devices {
		b.Devices[d.Hex()] = wrap
	}
	text, err := json.Marshal(b)
	if err != nil {
		return nostr.Event{}, err
	}
	content, err := k.Encrypt(ctx, string(text), me)
	if err != nil {
		return nostr.Event{}, err
	}
	event := nostr.Event{Kind: RootsKind, CreatedAt: max(now, r.At+1), Content: content, Tags: nostr.Tags{protected}}
	if err := k.SignEvent(ctx, &event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// OpenRoots checks that the citizen signed the event, and decrypts it with
// k, which is the citizen key or a signer that decrypts as the citizen.
func OpenRoots(ctx context.Context, k nostr.Keyer, citizen nostr.PubKey, event nostr.Event) (Roots, error) {
	if event.Kind != RootsKind {
		return Roots{}, fmt.Errorf("devices: kind %d is not a roots event", event.Kind)
	}
	if event.PubKey != citizen {
		return Roots{}, errors.New("devices: the citizen did not sign the roots")
	}
	if !event.VerifySignature() {
		return Roots{}, errors.New("devices: the signature of the roots is not valid")
	}
	text, err := k.Decrypt(ctx, event.Content, citizen)
	if err != nil {
		return Roots{}, fmt.Errorf("devices: the roots do not open: %w", err)
	}
	var b rootsBody
	if err := json.Unmarshal([]byte(text), &b); err != nil {
		return Roots{}, fmt.Errorf("devices: the roots are not valid: %w", err)
	}
	if b.Gen == 0 || b.Gen > MaxGenerations || len(b.Devices) > MaxDevices {
		return Roots{}, errors.New("devices: the roots break a limit")
	}
	r := Roots{Gen: b.Gen, Citizen: b.Citizen, Chain: b.Chain, At: event.CreatedAt, Devices: map[nostr.PubKey]string{}}
	for hexKey, wrap := range b.Devices {
		d, err := nostr.PubKeyFromHex(hexKey)
		if err != nil {
			return Roots{}, fmt.Errorf("devices: the roots name a bad device key: %w", err)
		}
		r.Devices[d] = wrap
	}
	return r, nil
}

// Unwrap opens the newest root with k: the citizen key opens the wrap to
// the citizen, and a device key opens the wrap to itself.
func (r Roots) Unwrap(ctx context.Context, k nostr.Keyer, citizen nostr.PubKey) (Root, error) {
	me, err := k.GetPublicKey(ctx)
	if err != nil {
		return Root{}, err
	}
	wrap := r.Citizen
	if me != citizen {
		var ok bool
		if wrap, ok = r.Devices[me]; !ok {
			return Root{}, fmt.Errorf("devices: generation %d has no wrap to this device key", r.Gen)
		}
	}
	plain, err := k.Decrypt(ctx, wrap, citizen)
	if err != nil {
		return Root{}, fmt.Errorf("devices: the wrap of generation %d does not open: %w", r.Gen, err)
	}
	return parseRoot(r.Gen, plain)
}

// Older opens the chain with the newest root, and returns each random root
// of an older generation.
func (r Roots) Older(newest Root) (map[uint32][32]byte, error) {
	out := map[uint32][32]byte{}
	if r.Chain == "" {
		return out, nil
	}
	ck, err := chainKey(newest.Key)
	if err != nil {
		return nil, err
	}
	text, err := nip44.Decrypt(r.Chain, ck)
	if err != nil {
		return nil, fmt.Errorf("devices: the chain does not open: %w", err)
	}
	var plain map[string]string
	if err := json.Unmarshal([]byte(text), &plain); err != nil {
		return nil, fmt.Errorf("devices: the chain is not valid: %w", err)
	}
	for g, h := range plain {
		gen, err := strconv.ParseUint(g, 10, 32)
		if err != nil || gen == 0 || gen >= uint64(newest.Gen) {
			return nil, fmt.Errorf("devices: the chain names a bad generation %q", g)
		}
		root, err := parseRoot(uint32(gen), h)
		if err != nil {
			return nil, err
		}
		out[root.Gen] = root.Key
	}
	return out, nil
}

// All returns each random root that r holds for k: the newest and the chain.
func (r Roots) All(ctx context.Context, k nostr.Keyer, citizen nostr.PubKey) (map[uint32][32]byte, error) {
	newest, err := r.Unwrap(ctx, k, citizen)
	if err != nil {
		return nil, err
	}
	all, err := r.Older(newest)
	if err != nil {
		return nil, err
	}
	all = maps.Clone(all)
	all[newest.Gen] = newest.Key
	return all, nil
}

func parseRoot(gen uint32, text string) (Root, error) {
	b, err := hex.DecodeString(text)
	if err != nil || len(b) != 32 {
		return Root{}, fmt.Errorf("devices: generation %d holds no root of 32 bytes", gen)
	}
	root := Root{Gen: gen}
	copy(root.Key[:], b)
	return root, nil
}

// RootsFilter matches the roots event of a citizen.
func RootsFilter(citizen nostr.PubKey) nostr.Filter {
	return nostr.Filter{Kinds: []nostr.Kind{RootsKind}, Authors: []nostr.PubKey{citizen}}
}

// SealData encrypts text for one app with its content key under root. The
// result names the generation in clear text: the generation in decimal, a
// colon, and the NIP-44 payload.
func SealData(root Root, app, text string) (string, error) {
	key, err := ContentKey(root.Key, app)
	if err != nil {
		return "", err
	}
	payload, err := nip44.Encrypt(text, key)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(root.Gen), 10) + ":" + payload, nil
}

// DataGeneration reads the generation that sealed data names.
func DataGeneration(sealed string) (uint32, error) {
	g, _, ok := strings.Cut(sealed, ":")
	gen, err := strconv.ParseUint(g, 10, 32)
	if !ok || err != nil {
		return 0, errors.New("devices: the data names no generation")
	}
	return uint32(gen), nil
}

// OpenData decrypts data that SealData sealed for app, with the root of its
// generation.
func OpenData(root [32]byte, app, sealed string) (string, error) {
	_, payload, ok := strings.Cut(sealed, ":")
	if !ok {
		return "", errors.New("devices: the data names no generation")
	}
	key, err := ContentKey(root, app)
	if err != nil {
		return "", err
	}
	text, err := nip44.Decrypt(payload, key)
	if err != nil {
		return "", fmt.Errorf("devices: the data does not open: %w", err)
	}
	return text, nil
}
