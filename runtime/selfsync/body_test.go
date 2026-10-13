package selfsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/keys"
	"golang.org/x/crypto/chacha20poly1305"
)

// memBodies keeps bodies in memory.
type memBodies map[string][]byte

func (m memBodies) Has(sum string) bool { _, ok := m[sum]; return ok }

func (m memBodies) Get(sum string) ([]byte, error) {
	data, ok := m[sum]
	if !ok {
		return nil, os.ErrNotExist
	}
	return data, nil
}

func (m memBodies) Put(sum string, data []byte) error {
	m[sum] = bytes.Clone(data)
	return nil
}

// countedBodies counts the reads of whole bodies.
type countedBodies struct {
	memBodies
	gets int
}

func (c *countedBodies) Get(sum string) ([]byte, error) {
	c.gets++
	return c.memBodies.Get(sum)
}

// fileWrap makes the wrap of a file with a body of n random bytes, as the
// files app does. It returns the wrap and the encrypted body.
func fileWrap(t *testing.T, citizen keys.Key, n int) (nostr.Event, []byte) {
	t.Helper()
	s := draft.Sealer{Citizen: citizen.Public, Signer: keyer.NewPlainKeySigner(citizen.Secret), Key: [32]byte{7}}
	raw := make([]byte, n)
	rand.Read(raw)
	sealed, ref, err := s.SealBody(raw)
	if err != nil {
		t.Fatal(err)
	}
	inner := nostr.Event{Kind: 1063, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"x", "00"}}}
	wrap, err := s.WrapBody(context.Background(), "file/1", inner, ref, nostr.Now())
	if err != nil {
		t.Fatal(err)
	}
	return wrap, sealed
}

// TestAFileTravelsAsOneBlob checks section 3.4 of the storage spec. The
// blob of a wrap with a body carries the body, coded into pieces as any
// blob. A second device gets the wrap and the body. A device that holds
// the wrap with no body, from a directory, gets the body from the
// pieces. The bytes on the storage citizens are at most twice the blob,
// and the blob at most twice the body.
func TestAFileTravelsAsOneBlob(t *testing.T) {
	ctx := context.Background()
	citizen := keys.Generate()
	wrap, body := fileWrap(t, citizen, 3<<20)
	ref, _ := draft.Body(wrap)

	sync := func(s *Syncer, sides []Side) Report {
		t.Helper()
		r, err := s.SyncAll(ctx, sides)
		if err != nil {
			t.Fatal(err)
		}
		for i, side := range r.Sides {
			if side.Err != nil {
				t.Fatalf("side %d: %v", i, side.Err)
			}
		}
		return r
	}
	stores := make([]*memStorage, 3)
	sides := make([]Side, 3)
	for i := range stores {
		stores[i] = newMemStorage()
		sides[i] = Side{Key: keys.Generate().Public, Label: fmt.Sprint("s", i), Conn: stores[i], Active: true}
	}

	// A device without the body puts nothing, and says why.
	lacking := testSyncer(t, citizen, &memStore{events: map[nostr.ID]nostr.Event{wrap.ID: wrap}})
	lacking.Bodies = memBodies{}
	r := sync(lacking, sides)
	if r.Sent != 0 || !strings.Contains(strings.Join(r.Refused, "\n"), "not on this device") {
		t.Fatalf("a device without the body coded %d records; refused %v", r.Sent, r.Refused)
	}

	writer := testSyncer(t, citizen, &memStore{events: map[nostr.ID]nostr.Event{wrap.ID: wrap}})
	counted := &countedBodies{memBodies: memBodies{ref.Sum: body}}
	writer.Bodies = counted
	if r := sync(writer, sides); r.Sent != 1 || r.K != 2 || r.T != 3 {
		t.Fatalf("the writer coded %d records as %d of %d; want 1 as 2 of 3", r.Sent, r.K, r.T)
	}
	// A sync with the blob on the storage citizens reads no body.
	gets := counted.gets
	sync(writer, sides)
	if counted.gets != gets {
		t.Fatalf("a sync with nothing to put read %d bodies", counted.gets-gets)
	}
	var held int64
	for _, s := range stores {
		for _, data := range s.data {
			held += int64(len(data))
		}
	}
	// 3 MiB + 40 pads to a blob of 4 MiB; three pieces of 2 MiB + 45.
	if blob := int64(4 << 20); held > 2*blob || blob > 2*int64(len(body)) {
		t.Fatalf("the storage citizens hold %d bytes for a body of %d", held, len(body))
	}

	reader := testSyncer(t, citizen, &memStore{events: map[nostr.ID]nostr.Event{}})
	got := memBodies{}
	reader.Bodies = got
	if r := sync(reader, sides); r.Received != 1 {
		t.Fatalf("the second device took %d records; refused %v", r.Received, r.Refused)
	}
	if !bytes.Equal(got[ref.Sum], body) {
		t.Fatal("the second device got another body")
	}

	carried := testSyncer(t, citizen, &memStore{events: map[nostr.ID]nostr.Event{wrap.ID: wrap}})
	gotCarried := memBodies{}
	carried.Bodies = gotCarried
	sync(carried, sides)
	if !bytes.Equal(gotCarried[ref.Sum], body) {
		t.Fatal("a device with the wrap and no body did not get the body")
	}
}

// sealRaw seals any envelope and body into a blob, as a device with the
// blob key and bad intent could.
func sealRaw(t *testing.T, k Keys, device nostr.SecretKey, env Envelope, body []byte) (string, []byte) {
	t.Helper()
	id := k.BlobID(env.Record.ID)
	digest := changeDigest(id, env.Record.ID)
	priv, _ := btcec.PrivKeyFromBytes(device[:])
	sig, err := schnorr.Sign(priv, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	env.Device, env.Sig = device.Public(), hex.EncodeToString(sig.Serialize())
	plain, _ := json.Marshal(env)
	size := blobSize(len(plain) + len(body))
	padded := make([]byte, size-int64(overheadSize))
	binary.BigEndian.PutUint64(padded, uint64(len(plain)))
	copy(padded[lengthSize:], plain)
	copy(padded[lengthSize+len(plain):], body)
	blob := make([]byte, headerSize+chacha20poly1305.NonceSizeX, size)
	raw, _ := hex.DecodeString(id)
	copy(blob, raw)
	binary.BigEndian.PutUint32(blob[40:], k.Gen)
	rand.Read(blob[headerSize:])
	aead, _ := chacha20poly1305.NewX(k.Blob)
	return id, aead.Seal(blob, blob[headerSize:], padded, blob[:headerSize])
}

// TestABlobMustCarryTheBodyOfItsWrap checks section 3.5 of the storage
// spec: a device refuses a blob whose body is not the body that the wrap
// names, and an envelope whose version does not fit its record.
func TestABlobMustCarryTheBodyOfItsWrap(t *testing.T) {
	citizen := keys.Generate()
	k := testKeys(t, 0)
	wrap, body := fileWrap(t, citizen, 10_000)

	if _, _, err := Seal(k, citizen.Secret, wrap, nil); err == nil {
		t.Error("Seal made the blob of a wrap with no body")
	}
	id, blob, err := Seal(k, citizen.Secret, wrap, body)
	if err != nil {
		t.Fatal(err)
	}
	if _, got, err := Open(k, id, blob); err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the blob opened with %d bytes of body: %v", len(got), err)
	}

	other := bytes.Clone(body)
	other[50] ^= 1
	for _, c := range []struct {
		name string
		env  Envelope
		body []byte
		want string
	}{
		{"another body", Envelope{Version: 2, Record: wrap, Body: int64(len(other))}, other, "does not hash"},
		{"a short body", Envelope{Version: 2, Record: wrap, Body: int64(len(body) - 1)}, body[:len(body)-1], "names"},
		{"version 1 with a body", Envelope{Version: 1, Record: wrap}, nil, "version 1 holds no body"},
		{"a body past the blob", Envelope{Version: 2, Record: wrap, Body: 1 << 30}, body, "larger than the blob"},
	} {
		id, blob := sealRaw(t, k, citizen.Secret, c.env, c.body)
		if _, _, err := Open(k, id, blob); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: Open said %v, want %q", c.name, err, c.want)
		}
	}

	page := nostr.Event{Kind: draft.Kind, CreatedAt: nostr.Now(), Tags: nostr.Tags{{"d", "page"}}, Content: "x"}
	page.Sign(citizen.Secret)
	id, blob = sealRaw(t, k, citizen.Secret, Envelope{Version: 2, Record: page, Body: 3}, []byte("abc"))
	if _, _, err := Open(k, id, blob); err == nil || !strings.Contains(err.Error(), "names none") {
		t.Errorf("a body for a record with no body tag: %v", err)
	}
}
