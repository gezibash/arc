// Package control holds the local answer to three questions: who is this
// identity, what is its name, and is it still valid.
//
// The entries live in ~/.config/arc/control, one file for each identity.
// Two commands on one machine therefore find each other without a relay.
//
// The entry of the Elixir release is written in the term format of Erlang.
// This package writes JSON and reads JSON. A citizen publishes itself when
// it starts, so an entry of the older format is replaced as soon as that
// citizen runs again.
package control

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gezibash/arc/identity"
)

// The states of an entry.
const (
	Active  = "active"
	Revoked = "revoked"
)

// ErrNotFound reports a query that names no one.
var ErrNotFound = errors.New("control: no identity answers to that")

// Entry is what the control plane knows about one identity.
type Entry struct {
	PublicKey   string `json:"public_key"`
	Name        string `json:"name"`
	ShortName   string `json:"short_name"`
	PublishedAt int64  `json:"published_at"`
	Status      string `json:"status"`
}

// Key returns the public key of an entry.
func (e *Entry) Key() ([]byte, error) { return hex.DecodeString(e.PublicKey) }

// Store is the control plane of this machine.
type Store struct {
	// Dir is the directory of ARC, for example ~/.config/arc.
	Dir string
}

// DefaultStore returns the store of this user.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(home, ".config", "arc")}, nil
}

func (s *Store) dir() string { return filepath.Join(s.Dir, "control") }

func (s *Store) path(publicKey []byte) string {
	return filepath.Join(s.dir(), hex.EncodeToString(publicKey)+".json")
}

// Publish writes the entry of one identity.
func (s *Store) Publish(me *identity.Identity) (*Entry, error) {
	entry := &Entry{
		PublicKey:   me.EncodePublicKey(),
		Name:        me.Name(),
		ShortName:   me.ShortName(),
		PublishedAt: time.Now().UnixMilli(),
		Status:      Active,
	}
	return entry, s.write(me.PublicKey, entry)
}

// Revoke marks an identity as one that no longer answers.
func (s *Store) Revoke(publicKey []byte) error {
	entry, err := s.Get(publicKey)
	if errors.Is(err, ErrNotFound) {
		entry = &Entry{
			PublicKey: hex.EncodeToString(publicKey),
			Name:      identity.Name(publicKey),
			ShortName: identity.ShortName(publicKey),
		}
	} else if err != nil {
		return err
	}

	entry.Status = Revoked
	entry.PublishedAt = time.Now().UnixMilli()
	return s.write(publicKey, entry)
}

// Get reads the entry of one identity.
func (s *Store) Get(publicKey []byte) (*Entry, error) {
	data, err := os.ReadFile(s.path(publicKey))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	entry := &Entry{}
	if err := json.Unmarshal(data, entry); err != nil {
		return nil, ErrNotFound
	}
	return entry, nil
}

// List returns every entry, in name order.
func (s *Store) List() ([]*Entry, error) {
	files, err := os.ReadDir(s.dir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var entries []*Entry
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		data, err := os.ReadFile(filepath.Join(s.dir(), file.Name()))
		if err != nil {
			continue
		}

		entry := &Entry{}
		if err := json.Unmarshal(data, entry); err == nil && entry.PublicKey != "" {
			entries = append(entries, entry)
		}
	}

	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Name < entries[right].Name
	})
	return entries, nil
}

// Resolve finds the identities that answer to a petname, a short petname, or
// the start of a public key. A revoked identity never answers.
func (s *Store) Resolve(query string) ([]*Entry, error) {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return nil, ErrNotFound
	}

	entries, err := s.List()
	if err != nil {
		return nil, err
	}

	var found []*Entry
	for _, entry := range entries {
		if entry.Status == Revoked {
			continue
		}
		if strings.ToLower(entry.Name) == query ||
			strings.ToLower(entry.ShortName) == query ||
			strings.HasPrefix(entry.PublicKey, query) {
			found = append(found, entry)
		}
	}

	if len(found) == 0 {
		return nil, ErrNotFound
	}
	return found, nil
}

func (s *Store) write(publicKey []byte, entry *Entry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir(), 0o700); err != nil {
		return err
	}
	return os.WriteFile(s.path(publicKey), data, 0o600)
}
