// Package vectors reads the shared protocol vectors.
//
// The file test/vectors/identity.json holds values that every ARC
// implementation must produce. The Elixir suite reads the same file. A change
// to a vector is a change to the protocol.
package vectors

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// Identity holds one identity and everything that follows from its seed.
type Identity struct {
	Seed           string `json:"seed"`
	PublicKey      string `json:"public_key"`
	X25519Public   string `json:"x25519_public"`
	X25519Secret   string `json:"x25519_secret"`
	Name           string `json:"name"`
	ShortName      string `json:"short_name"`
	SignatureOfArc string `json:"signature_of_arc"`
}

// HKDF holds one key derivation.
type HKDF struct {
	IKM    string `json:"ikm"`
	Salt   string `json:"salt"`
	Info   string `json:"info"`
	Length int    `json:"length"`
	Output string `json:"output"`
}

// SealedBox holds one sealed message and the text inside it.
type SealedBox struct {
	RecipientSeed string `json:"recipient_seed"`
	Plaintext     string `json:"plaintext"`
	Sealed        string `json:"sealed"`
}

// Session holds one real session, and one packet of that session.
type Session struct {
	Version         int    `json:"version"`
	InitiatorSeed   string `json:"initiator_seed"`
	ResponderSeed   string `json:"responder_seed"`
	EphemeralPublic string `json:"ephemeral_public"`
	SessionID       string `json:"session_id"`
	SessionKey      string `json:"session_key"`
	Seq             uint64 `json:"seq"`
	TS              int64  `json:"ts"`
	Plaintext       string `json:"plaintext"`
	Packet          string `json:"packet"`
}

// Announcement holds one signed announcement and the time to check it at.
type Announcement struct {
	SignerSeed string         `json:"signer_seed"`
	Now        int64          `json:"now"`
	Record     map[string]any `json:"record"`
}

// File holds every vector of one version.
type File struct {
	Version      int          `json:"version"`
	Identities   []Identity   `json:"identities"`
	HKDF         []HKDF       `json:"hkdf"`
	SealedBox    SealedBox    `json:"sealed_box"`
	Session      Session      `json:"session"`
	Announcement Announcement `json:"announcement"`
}

// Load reads the vectors. It fails the test when the file is missing, because
// a test without its vectors proves nothing.
func Load(t *testing.T) File {
	t.Helper()

	path := filepath.Join("..", "..", "test", "vectors", "identity.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vectors: %v. Run: mix run --no-start scripts/write-vectors.exs", err)
	}

	// The numbers of a record keep their digits, because a signature covers
	// the canonical form of the record that arrived.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()

	var file File
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	return file
}
