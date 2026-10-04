package capability

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface"
)

// Provider is a validated execution and announcement definition. Runtime code
// consumes this one model; historical document formats stay at the loader.
type Provider struct {
	Interactions []session.Mode
	ID           string
	MaxBytes     int
	Content      string
	Terms        []string
}

// LoadProvider accepts an interface manifest on its own. For old bundles,
// an adjacent interface.json supersedes the historical manifest, even when
// that historical document is missing or invalid.
func LoadProvider(path string) (*Provider, error) {
	body, readErr := os.ReadFile(path)
	var version map[string]json.RawMessage
	_ = json.Unmarshal(body, &version)
	if _, ok := version["interface"]; ok {
		return currentProvider(body)
	}
	sibling := filepath.Join(filepath.Dir(path), "interface.json")
	if filepath.Clean(sibling) != filepath.Clean(path) {
		modern, err := os.ReadFile(sibling)
		if err == nil {
			return currentProvider(modern)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if readErr != nil {
		return nil, readErr
	}
	legacy, err := LoadFile(path)
	if err != nil {
		return nil, err
	}
	fields, _ := legacy["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)
	limit := 1024 * 1024
	if body, ok := invocation["request_body"].(map[string]any); ok {
		if v, ok := body["max_bytes"]; ok {
			n, valid := wholeNumber(v)
			if !valid || n <= 0 || n > int64(^uint(0)>>1) {
				return nil, fmt.Errorf("capability: invalid request body limit")
			}
			limit = int(n)
		}
	}
	encoded, err := json.Marshal(legacy)
	if err != nil {
		return nil, err
	}
	var terms []string
	for _, name := range []string{"id", "scheme", "kind"} {
		if text := firstString(fields[name]); text != "" {
			terms = append(terms, text)
		}
	}
	return &Provider{ID: firstString(fields["id"]), MaxBytes: limit, Content: string(encoded), Terms: terms}, nil
}

func currentProvider(body []byte) (*Provider, error) {
	m, err := iface.Parse(body)
	if err != nil {
		return nil, err
	}
	if m.Shape != "service" || m.Service == nil {
		return nil, errors.New("capability: a provider must describe a service")
	}
	limit := m.Service.MaxBytes
	if limit == 0 {
		limit = 1024 * 1024
	}
	m.Service.MaxBytes = limit
	encoded, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return &Provider{Interactions: m.Service.Interactions, ID: m.ID, MaxBytes: limit, Content: string(encoded), Terms: []string{m.ID, m.Shape}}, nil
}
