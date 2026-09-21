package toolbox

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

// The owner of a citizen decides which signers it trusts. A decision is
// global: it holds for every capability that signer publishes.
//
// A signer that the owner denied never installs again without a new
// decision. A signer that the owner allowed installs without a question.

// The two states that an owner records.
const (
	Allowed = "allowed"
	Denied  = "denied"
)

// Signer is one decision of the owner.
type Signer struct {
	PublicKey      string `json:"signer_public_key"`
	State          string `json:"state"`
	Scope          string `json:"scope"`
	FirstTrustedAt int64  `json:"first_trusted_at"`
	Note           string `json:"note,omitempty"`
}

type trustDocument struct {
	Version        int      `json:"version"`
	OwnerName      string   `json:"owner_name"`
	OwnerPublicKey string   `json:"owner_public_key"`
	Signers        []Signer `json:"signers"`
}

func (s *Store) trustPath(owner []byte) string {
	return filepath.Join(s.Dir, "trust", hex.EncodeToString(owner), "signers.json")
}

// Signers returns every decision of this owner, in key order.
func (s *Store) Signers(owner []byte) ([]Signer, error) {
	held, err := s.loadTrust(owner)
	if err != nil {
		return nil, err
	}
	return held.Signers, nil
}

// TrustState returns what the owner decided about one signer, or the empty
// string when the owner has not decided.
func (s *Store) TrustState(owner []byte, signer string) (string, error) {
	signers, err := s.Signers(owner)
	if err != nil {
		return "", err
	}

	signer = strings.ToLower(strings.TrimSpace(signer))
	for _, held := range signers {
		if held.PublicKey == signer {
			return held.State, nil
		}
	}
	return "", nil
}

// Trust records a decision of the owner about one signer.
func (s *Store) Trust(owner []byte, signer, state, note string) (*Signer, error) {
	if state != Allowed && state != Denied {
		return nil, errors.New("toolbox: a decision is allowed or denied")
	}

	signer = strings.ToLower(strings.TrimSpace(signer))
	if raw, err := hex.DecodeString(signer); err != nil || len(raw) != 32 {
		return nil, errors.New("toolbox: a signer is 64 characters of hex")
	}

	held, err := s.loadTrust(owner)
	if err != nil {
		return nil, err
	}

	record := Signer{
		PublicKey: signer, State: state, Scope: "global",
		FirstTrustedAt: time.Now().UnixMilli(), Note: note,
	}

	signers := make([]Signer, 0, len(held.Signers)+1)
	for _, existing := range held.Signers {
		if existing.PublicKey == signer {
			// The first decision keeps its time.
			record.FirstTrustedAt = existing.FirstTrustedAt
			continue
		}
		signers = append(signers, existing)
	}

	signers = append(signers, record)
	sort.Slice(signers, func(left, right int) bool {
		return signers[left].PublicKey < signers[right].PublicKey
	})

	held.Signers = signers
	if err := s.saveTrust(owner, held); err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *Store) loadTrust(owner []byte) (*trustDocument, error) {
	held := &trustDocument{
		Version:        1,
		OwnerName:      identity.Name(owner),
		OwnerPublicKey: hex.EncodeToString(owner),
		Signers:        []Signer{},
	}

	data, err := os.ReadFile(s.trustPath(owner))
	if errors.Is(err, os.ErrNotExist) {
		return held, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, held); err != nil {
		return nil, errors.New("toolbox: the trust file does not hold together")
	}
	if held.Signers == nil {
		held.Signers = []Signer{}
	}
	return held, nil
}

func (s *Store) saveTrust(owner []byte, held *trustDocument) error {
	held.Version = 1
	held.OwnerName = identity.Name(owner)
	held.OwnerPublicKey = hex.EncodeToString(owner)

	data, err := json.Marshal(held)
	if err != nil {
		return err
	}

	path := s.trustPath(owner)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}
