package selfsync

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/sdk/storage"
	"github.com/klauspost/reedsolomon"
)

// testKeys returns the keys of generation gen with the roots of
// TestNewKeys.
func testKeys(t *testing.T, gen uint32) Keys {
	t.Helper()
	keyed := make([]byte, 32)
	data := make([]byte, 32)
	for i := range keyed {
		keyed[i] = byte(i)
		data[i] = byte(0xff - i)
	}
	root := data
	if gen == 0 {
		root = keyed
	}
	k, err := NewKeys(gen, root, keyed)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// TestPieceKeys checks the keys of section 3.5 of the storage spec. The
// expected values come from Python's hmac and hashlib, as in TestNewKeys:
//
//	hkdf(data, b"arc-storage-piece-v1")
//	hkdf(keyed, b"arc-storage-piece-id-v1" + b"\xaa" * 32)
//	hkdf(keyed, b"arc-storage-place-v1")
//	hmac.new(piece_id_key, bytes(range(100, 132)) + bytes([2]), sha256)
func TestPieceKeys(t *testing.T) {
	const (
		pieceOfData  = "df9dbf87edef6efb25be2fba3a4c01399d89a28a6a7dd743f726a70491ed2f8f"
		pieceOfKeyed = "66b3f5225aedab409b9b4112a452451157429fa64bcfb94484af527f538b1121"
		pieceIDKey   = "ee089f199107759c3eb3a1c8d1c4ae41cc3ae3e24fa02a0daa99181402cd38aa"
		placeKey     = "1a5d594d4000f97061bf6fd02c78b8365f3eb5daef9f4784025d914ff68df4a3"
		pieceID      = "d6269e5cdab5f1de3ce7944cebf35a1f2fae8dd5449ee54f4d7f526a7385f11e"
	)
	zero, two := testKeys(t, 0), testKeys(t, 2)
	if got := hex.EncodeToString(zero.Piece); got != pieceOfKeyed {
		t.Fatalf("the piece key of generation 0 is %s, want %s", got, pieceOfKeyed)
	}
	if got := hex.EncodeToString(two.Piece); got != pieceOfData {
		t.Fatalf("the piece key of generation 2 is %s, want %s", got, pieceOfData)
	}
	for _, k := range []Keys{zero, two} {
		if got := hex.EncodeToString(k.Place); got != placeKey {
			t.Fatalf("the placement key of generation %d is %s, want %s", k.Gen, got, placeKey)
		}
		var sk nostr.PubKey
		for i := range sk {
			sk[i] = 0xaa
		}
		idKey, err := k.PieceIDKey(sk)
		if err != nil {
			t.Fatal(err)
		}
		if got := hex.EncodeToString(idKey); got != pieceIDKey {
			t.Fatalf("the piece ID key of generation %d is %s, want %s", k.Gen, got, pieceIDKey)
		}
		blob := make([]byte, 32)
		for i := range blob {
			blob[i] = byte(100 + i)
		}
		if got := PieceID(idKey, hex.EncodeToString(blob), 2); got != pieceID {
			t.Fatalf("the piece ID is %s, want %s", got, pieceID)
		}
	}
}

// TestCode checks decision 6 of the storage network proposal against its
// table: k = ceil(S/2) and t = min(S, 2k).
func TestCode(t *testing.T) {
	want := map[int][2]int{1: {1, 1}, 2: {1, 2}, 3: {2, 3}, 4: {2, 4}, 5: {3, 5}, 6: {3, 6}, 16: {8, 16}}
	for s, kt := range want {
		if k, n := Code(s); k != kt[0] || n != kt[1] {
			t.Fatalf("S = %d gives k = %d and t = %d, want %d and %d", s, k, n, kt[0], kt[1])
		}
	}
}

// TestPieces seals a blob into pieces for each k, and rebuilds it from
// each set of k pieces. A changed byte, or a piece with the tag of another
// piece, does not open.
func TestPieces(t *testing.T) {
	keys := testKeys(t, 2)
	for _, size := range []int{storage.MinBlob, 64 << 10} {
		blob := make([]byte, size)
		_, _ = rand.Read(blob)
		id := hex.EncodeToString(blob[:32])
		for k := 1; k <= 4; k++ {
			shards, err := Shards(blob, k)
			if err != nil {
				t.Fatal(err)
			}
			seal, _ := NewSeal()
			tags, pieces := make([][]byte, 2*k), make([][]byte, 2*k)
			for i := range 2 * k {
				h := Header{Blob: id, Seal: seal, K: k, Index: i, Size: int64(size)}
				tags[i], pieces[i], err = SealPiece(keys, h, shards[i])
				if err != nil {
					t.Fatal(err)
				}
				if len(tags[i]) != storage.TagSize {
					t.Fatalf("a tag has %d bytes, want %d", len(tags[i]), storage.TagSize)
				}
				if want := storage.PieceSize(int64(size), k); int64(len(pieces[i])) != want || !storage.GoodPieceSize(want) {
					t.Fatalf("k = %d: a piece has %d bytes, want %d", k, len(pieces[i]), want)
				}
				if k > 1 && bytes.Contains(pieces[i], blob[:64]) {
					t.Fatalf("k = %d: piece %d holds the bytes of the blob", k, i)
				}
			}
			// Each window of k indices rebuilds the blob.
			for first := 0; first+k <= 2*k; first++ {
				got := map[int][]byte{}
				for i := first; i < first+k; i++ {
					h, err := OpenTag(keys, tags[i])
					if err != nil {
						t.Fatal(err)
					}
					if h.Blob != id || h.Seal != seal || h.K != k || h.Index != i || h.Size != int64(size) {
						t.Fatalf("the tag opens to %+v", h)
					}
					if got[i], err = OpenPiece(keys, tags[i], h, pieces[i]); err != nil {
						t.Fatal(err)
					}
				}
				h, _ := OpenTag(keys, tags[first])
				rebuilt, err := Rebuild(h, got)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(rebuilt, blob) {
					t.Fatalf("k = %d: pieces %d to %d rebuild other bytes", k, first, first+k-1)
				}
			}
			h, _ := OpenTag(keys, tags[0])
			changed := bytes.Clone(pieces[0])
			changed[len(changed)/2] ^= 1
			if _, err := OpenPiece(keys, tags[0], h, changed); err == nil {
				t.Fatalf("k = %d: a piece with a changed byte opens", k)
			}
			if _, err := OpenPiece(keys, tags[1], h, pieces[0]); err == nil {
				t.Fatalf("k = %d: a piece opens with the tag of another piece", k)
			}
			badTag := bytes.Clone(tags[0])
			badTag[40] ^= 1
			if _, err := OpenTag(keys, badTag); err == nil {
				t.Fatalf("k = %d: a tag with a changed byte opens", k)
			}
		}
	}
	// A revoked device holds an older root. It opens no piece of a newer
	// generation.
	blob := make([]byte, storage.MinBlob)
	shards, _ := Shards(blob, 2)
	tag, _, err := SealPiece(keys, Header{Blob: hex.EncodeToString(blob[:32]), K: 2, Size: storage.MinBlob}, shards[0])
	if err != nil {
		t.Fatal(err)
	}
	older := testKeys(t, 2)
	older.Piece = testKeys(t, 0).Piece
	if _, err := OpenTag(older, tag); err == nil {
		t.Fatal("a tag of generation 2 opens with the piece key of generation 0")
	}
}

// TestPieceIDsDiffer checks decision 1: two storage citizens see no common
// piece ID for the pieces of one blob.
func TestPieceIDsDiffer(t *testing.T) {
	keys := testKeys(t, 0)
	blob := hex.EncodeToString(bytes.Repeat([]byte{7}, 32))
	seen := map[string]string{}
	for s := range 3 {
		var key nostr.PubKey
		key[0] = byte(s + 1)
		idKey, err := keys.PieceIDKey(key)
		if err != nil {
			t.Fatal(err)
		}
		for index := range 4 {
			id := PieceID(idKey, blob, index)
			if other, ok := seen[id]; ok {
				t.Fatalf("storage citizen %d, index %d has the ID of %s", s, index, other)
			}
			seen[id] = "another piece"
		}
	}
}

// TestParityPrefix keeps the prefix property of the pinned library: the
// shards of a (k, n) code are the first n shards of a (k, m) code for m >
// n. The device uses RS(k, 2k) and does not depend on it. A newer version
// that breaks it changes the bytes of each piece of index k or more, so a
// device of that version could not rebuild older pieces with them.
func TestParityPrefix(t *testing.T) {
	blob := make([]byte, 64<<10)
	_, _ = rand.Read(blob)
	for _, c := range [][3]int{{1, 1, 2}, {2, 3, 4}, {2, 3, 6}, {3, 5, 6}, {3, 4, 8}, {4, 6, 8}} {
		k, n, m := c[0], c[1], c[2]
		a, err := reedsolomon.New(k, n-k)
		if err != nil {
			t.Fatalf("k=%d n=%d: %v", k, n, err)
		}
		b, err := reedsolomon.New(k, m-k)
		if err != nil {
			t.Fatal(err)
		}
		sa, _ := a.Split(bytes.Clone(blob))
		if err := a.Encode(sa); err != nil {
			t.Fatal(err)
		}
		sb, _ := b.Split(bytes.Clone(blob))
		if err := b.Encode(sb); err != nil {
			t.Fatal(err)
		}
		for i := range n {
			if !bytes.Equal(sa[i], sb[i]) {
				t.Fatalf("k=%d: shard %d of the (%d, %d) code differs from shard %d of the (%d, %d) code", k, i, k, n, i, k, m)
			}
		}
	}
	// The device codes with RS(k, 2k): its shards are also those of the
	// smallest code.
	shards, err := Shards(blob, 2)
	if err != nil {
		t.Fatal(err)
	}
	small, _ := reedsolomon.New(2, 1)
	ss, _ := small.Split(bytes.Clone(blob))
	_ = small.Encode(ss)
	for i := range 3 {
		if !bytes.Equal(ss[i], shards[i]) {
			t.Fatalf("shard %d of RS(2, 4) differs from shard %d of RS(2, 3)", i, i)
		}
	}
}
