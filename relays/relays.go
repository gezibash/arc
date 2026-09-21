// Package relays holds the pinned relay selection of this machine.
//
// The file ~/.config/arc/relays.json holds only public values: the address of
// each relay that the user joined, and the public key that the user pinned
// for it.
//
//	{"version":1,"default":"relay.example:7331",
//	 "relays":{"relay.example:7331":"<64 characters of hex>"}}
//
// A pin that changes is a trust failure, and never an update.
package relays

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// DefaultPort is the port of a relay that names none.
const DefaultPort = 7331

// MaxFileBytes caps the file.
const MaxFileBytes = 16 * 1024

// Errors of this package.
var (
	ErrInvalidAddress = errors.New("relays: the address must be host:port")
	ErrInvalidPin     = errors.New("relays: the pin must be 64 characters of hex")
	ErrPinMismatch    = errors.New("relays: the relay answers with another key than the pinned key")
	ErrNoRelay        = errors.New("relays: no relay: join one with arc join")
	ErrNotPrivate     = errors.New("relays: the file must be readable by its owner only")
)

// Document is the file.
type Document struct {
	Version int               `json:"version"`
	Default string            `json:"default,omitempty"`
	Relays  map[string]string `json:"relays"`
}

// Store is one directory of ARC.
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

// Path is the file that holds the relays.
func (s *Store) Path() string { return filepath.Join(s.Dir, "relays.json") }

// Load reads the file. A file that is not there is an empty document.
func (s *Store) Load() (*Document, error) {
	info, err := os.Lstat(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return &Document{Version: 1, Relays: map[string]string{}}, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > MaxFileBytes {
		return nil, ErrNotPrivate
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, ErrNotPrivate
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		return nil, err
	}

	document := &Document{}
	if err := json.Unmarshal(data, document); err != nil {
		return nil, fmt.Errorf("relays: %s is not valid JSON", s.Path())
	}
	if document.Relays == nil {
		document.Relays = map[string]string{}
	}
	return document, nil
}

// Remember saves one relay and makes it the default. A pin that differs from
// the pin held is an error.
func (s *Store) Remember(address, pin string) error {
	canonical, err := NormalizeAddress(address)
	if err != nil {
		return err
	}
	key, err := NormalizePin(pin)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	document, err := s.Load()
	if err != nil {
		return err
	}
	if held, ok := document.Relays[canonical]; ok && held != key {
		return ErrPinMismatch
	}

	document.Version = 1
	document.Default = canonical
	document.Relays[canonical] = key

	data, err := json.Marshal(document)
	if err != nil {
		return err
	}

	temporary := s.Path() + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.Path())
}

// lock holds relays.json.lock for one read and one write, so that two joins
// at the same time never lose a change. The operating system releases the
// lock when the process ends, so a crash leaves no stale lock.
func (s *Store) lock() (func(), error) {
	file, err := os.OpenFile(filepath.Join(s.Dir, "relays.json.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, err
	}
	return func() {
		syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		file.Close()
	}, nil
}

// Selection is the relay that a command uses.
type Selection struct {
	Address string
	Pin     []byte
}

// Resolve picks the relay: the flags first, then ARC_RELAY and
// ARC_RELAY_PUBKEY, then the relay that the user joined.
func (s *Store) Resolve(address, pin string) (*Selection, error) {
	if address == "" {
		address = os.Getenv("ARC_RELAY")
	}
	if pin == "" {
		pin = os.Getenv("ARC_RELAY_PUBKEY")
	}

	document, err := s.Load()
	if err != nil {
		return nil, err
	}

	if address == "" {
		address = document.Default
	}
	if address == "" {
		return nil, ErrNoRelay
	}

	canonical, err := NormalizeAddress(address)
	if err != nil {
		return nil, err
	}
	if pin == "" {
		pin = document.Relays[canonical]
	}
	if pin == "" {
		return &Selection{Address: canonical}, nil
	}

	key, err := NormalizePin(pin)
	if err != nil {
		return nil, err
	}

	raw, err := hex.DecodeString(key)
	if err != nil {
		return nil, ErrInvalidPin
	}
	return &Selection{Address: canonical, Pin: raw}, nil
}

// NormalizeAddress returns the address as host:port. A host without a port
// takes the default port.
func NormalizeAddress(address string) (string, error) {
	address = strings.TrimSpace(address)
	if address == "" || strings.ContainsAny(address, " \t\n\r/@?#") {
		return "", ErrInvalidAddress
	}

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		host, port = address, strconv.Itoa(DefaultPort)
	}
	if host == "" {
		return "", ErrInvalidAddress
	}

	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", ErrInvalidAddress
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(number)), nil
}

// NormalizePin returns the pin in lower case hex.
func NormalizePin(pin string) (string, error) {
	pin = strings.ToLower(strings.TrimSpace(pin))
	raw, err := hex.DecodeString(pin)
	if err != nil || len(raw) != 32 {
		return "", ErrInvalidPin
	}
	return pin, nil
}
