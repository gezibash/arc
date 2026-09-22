package identity

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// A key store holds one file for each identity, named by its petname:
//
//	~/.config/arc/keys/<petname>.toml
//	[identity]
//	seed = "<64 characters of hex>"
//
// The active identity comes from ARC_KEY, then arc.key in the current
// directory, then ~/.config/arc/default.key. Each of these holds a petname or
// a prefix of one, and never a secret. Older installations wrote
// ~/.config/arc/default_key; the store reads it only when default.key is
// absent.

// Errors of the key store.
var (
	ErrNotFound  = errors.New("identity: no key of that name")
	ErrAmbiguous = errors.New("identity: the name fits more than one key")
	ErrNoDefault = errors.New("identity: no default key")
	ErrEmpty     = errors.New("identity: the selector is empty")
)

var seedLine = regexp.MustCompile(`(?m)^\s*seed\s*=\s*"([0-9a-fA-F]{64})"\s*$`)

// Store is one directory of keys.
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

// KeysDir is the directory that holds the key files.
func (s *Store) KeysDir() string { return filepath.Join(s.Dir, "keys") }

// DefaultFile is the file that names the default key.
func (s *Store) DefaultFile() string { return filepath.Join(s.Dir, "default.key") }

// LegacyDefaultFile is the file that older installations wrote in place of
// DefaultFile.
func (s *Store) LegacyDefaultFile() string { return filepath.Join(s.Dir, "default_key") }

// Generate makes a new identity and saves it.
func (s *Store) Generate() (*Identity, error) {
	me, err := Generate()
	if err != nil {
		return nil, err
	}
	if err := s.Save(me); err != nil {
		return nil, err
	}
	return me, nil
}

// Save writes one identity to the store.
func (s *Store) Save(me *Identity) error {
	if err := os.MkdirAll(s.KeysDir(), 0o700); err != nil {
		return err
	}

	body := fmt.Sprintf("[identity]\nseed = \"%s\"\n", me.EncodeSeed())
	return os.WriteFile(filepath.Join(s.KeysDir(), me.Name()+".toml"), []byte(body), 0o600)
}

// List returns every identity of the store, in name order.
func (s *Store) List() ([]*Identity, error) {
	entries, err := os.ReadDir(s.KeysDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".toml") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	identities := make([]*Identity, 0, len(names))
	for _, name := range names {
		me, err := s.load(filepath.Join(s.KeysDir(), name))
		if err != nil {
			continue
		}
		identities = append(identities, me)
	}
	return identities, nil
}

// Get finds one identity by its petname, or by a prefix that fits one key.
func (s *Store) Get(nameOrPrefix string) (*Identity, error) {
	identities, err := s.List()
	if err != nil {
		return nil, err
	}

	for _, me := range identities {
		if me.Name() == nameOrPrefix {
			return me, nil
		}
	}

	var found []*Identity
	for _, me := range identities {
		if strings.HasPrefix(me.Name(), nameOrPrefix) {
			found = append(found, me)
		}
	}

	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return nil, ErrNotFound
	default:
		return nil, ErrAmbiguous
	}
}

// GetByPublicKey finds one identity by its public key.
func (s *Store) GetByPublicKey(publicKey []byte) (*Identity, error) {
	identities, err := s.List()
	if err != nil {
		return nil, err
	}

	for _, me := range identities {
		if string(me.PublicKey) == string(publicKey) {
			return me, nil
		}
	}
	return nil, ErrNotFound
}

// Remove deletes one key. It also clears the default when that key was the
// default.
func (s *Store) Remove(nameOrPrefix string) error {
	me, err := s.Get(nameOrPrefix)
	if err != nil {
		return err
	}

	wasDefault := false
	if current, err := s.Default(); err == nil && string(current.PublicKey) == string(me.PublicKey) {
		wasDefault = true
	}

	if err := os.Remove(filepath.Join(s.KeysDir(), me.Name()+".toml")); err != nil {
		return err
	}
	if wasDefault {
		os.Remove(s.DefaultFile())
		os.Remove(s.LegacyDefaultFile())
	}
	return nil
}

// SetDefault writes the name of the default key. After the write, it removes
// the legacy selector, so that an older file never names another key.
func (s *Store) SetDefault(nameOrPrefix string) error {
	me, err := s.Get(nameOrPrefix)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(s.DefaultFile(), []byte(me.Name()), 0o600); err != nil {
		return err
	}
	if err := os.Remove(s.LegacyDefaultFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Default returns the default identity.
func (s *Store) Default() (*Identity, error) {
	me, _, err := s.defaultIdentity()
	return me, err
}

// defaultIdentity reads default.key, or the legacy default_key when
// default.key is absent.
func (s *Store) defaultIdentity() (*Identity, Source, error) {
	for _, held := range []struct {
		path   string
		source Source
	}{
		{s.DefaultFile(), FromDefault},
		{s.LegacyDefaultFile(), FromLegacyDefault},
	} {
		name, present, err := readSelector(held.path)
		if err != nil {
			return nil, held.source, err
		}
		if present {
			me, err := s.Select(held.source, name)
			return me, held.source, err
		}
	}
	return nil, FromDefault, ErrNoDefault
}

// Source says which selector chose the active identity.
type Source string

// The selectors, in the order that the store reads them.
const (
	FromEnvironment   Source = "ARC_KEY"
	FromDirectory     Source = "arc.key"
	FromDefault       Source = "default.key"
	FromLegacyDefault Source = "default_key"
)

// Active resolves the identity of this shell: ARC_KEY, then arc.key in the
// current directory, then the default key. A selector that is there but does
// not fit a key is an error, and never falls through. Only a selector that
// is absent lets the next one answer.
func (s *Store) Active() (*Identity, Source, error) {
	if name := os.Getenv("ARC_KEY"); name != "" {
		me, err := s.Select(FromEnvironment, name)
		return me, FromEnvironment, err
	}

	name, present, err := readSelector("arc.key")
	if err != nil {
		return nil, FromDirectory, err
	}
	if present {
		me, err := s.Select(FromDirectory, name)
		return me, FromDirectory, err
	}

	return s.defaultIdentity()
}

// Select finds the key that one selector names. Its errors say which
// selector named what.
func (s *Store) Select(source Source, name string) (*Identity, error) {
	if name == "" {
		return nil, fmt.Errorf("%w: %s names no key", ErrEmpty, source)
	}

	me, err := s.Get(name)
	switch {
	case errors.Is(err, ErrNotFound):
		return nil, fmt.Errorf("%w: %s names %q", ErrNotFound, source, name)
	case errors.Is(err, ErrAmbiguous):
		return nil, fmt.Errorf("%w: %s names %q", ErrAmbiguous, source, name)
	}
	return me, err
}

func (s *Store) load(path string) (*Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	found := seedLine.FindSubmatch(data)
	if found == nil {
		return nil, fmt.Errorf("identity: %s holds no seed", filepath.Base(path))
	}
	return FromSeedHex(strings.ToLower(string(found[1])))
}

// readSelector reads a file that names a key. It says whether the file is
// there: only a file that is absent lets the next selector answer. A file
// that is there and empty names no key.
func readSelector(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", true, err
	}
	return strings.TrimSpace(string(data)), true, nil
}
