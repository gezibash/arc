// Package catalog announces capabilities, finds them, and remembers which
// ones a citizen trusts.
//
// A provider announces a capability with an addressable event of kind 30272.
// Its d tag is the capability id, and its content is the manifest. The
// provider signs the event, so the signature is the authorship of the
// manifest, and a new version replaces the old one.
package catalog

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/identity"
)

// Kind is the kind of a capability announcement.
const Kind nostr.Kind = 30272

// Offer is one announced capability.
type Offer struct {
	Provider nostr.PubKey
	ID       string
	Title    string
	Summary  string
	Scheme   string
	// Invocation is the method and path that a call carries by default.
	Method string
	Path   string
	// Package is the whole manifest, as the provider announced it.
	Package map[string]any
	Event   nostr.Event
}

// Name is the petname of the provider.
func (o Offer) Name() string { return identity.Name(o.Provider[:]) }

// Announce makes the announcement of one capability package. The package is
// the normalized manifest; its capability id becomes the d tag.
func Announce(k keys.Key, pkg map[string]any, at nostr.Timestamp) (nostr.Event, error) {
	fields, _ := pkg["capability"].(map[string]any)
	id, _ := fields["id"].(string)
	if id == "" {
		return nostr.Event{}, errors.New("catalog: the manifest names no capability id")
	}

	body, err := json.Marshal(pkg)
	if err != nil {
		return nostr.Event{}, err
	}

	tags := nostr.Tags{{"d", id}}
	for _, word := range terms(fields) {
		tags = append(tags, nostr.Tag{"t", word})
	}

	event := nostr.Event{Kind: Kind, CreatedAt: at, Tags: tags, Content: string(body)}
	if err := event.Sign(k.Secret); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// terms are the search words of a capability: its id, scheme and kind.
func terms(fields map[string]any) []string {
	var out []string
	for _, key := range []string{"id", "scheme", "kind"} {
		if word, _ := fields[key].(string); word != "" && !slices.Contains(out, strings.ToLower(word)) {
			out = append(out, strings.ToLower(word))
		}
	}
	return out
}

// Read opens one announcement. It checks the signature, and that the d tag
// names the capability in the manifest.
func Read(event nostr.Event) (Offer, error) {
	if event.Kind != Kind {
		return Offer{}, fmt.Errorf("catalog: kind %d is not an announcement", event.Kind)
	}
	if err := store.Verify(event); err != nil {
		return Offer{}, fmt.Errorf("catalog: %w", err)
	}

	var pkg map[string]any
	if err := json.Unmarshal([]byte(event.Content), &pkg); err != nil {
		return Offer{}, errors.New("catalog: the announcement holds no manifest")
	}
	fields, _ := pkg["capability"].(map[string]any)
	id, _ := fields["id"].(string)
	if id == "" || id != event.Tags.GetD() {
		return Offer{}, errors.New("catalog: the announcement names another capability than its manifest")
	}

	invocation, _ := fields["invocation"].(map[string]any)
	offer := Offer{
		Provider: event.PubKey, ID: id, Package: pkg, Event: event,
		Title: text(fields["title"]), Summary: text(fields["summary"]), Scheme: text(fields["scheme"]),
		Method: strings.ToUpper(text(invocation["method"])), Path: text(invocation["path"]),
	}
	if offer.Path == "" {
		offer.Path = "/"
	}
	return offer, nil
}

func text(v any) string {
	s, _ := v.(string)
	return s
}

// Search returns the offers in a store that match a query. An empty query
// matches every offer. A query matches the id, the scheme, the title, the
// summary, or the provider's petname, without regard to case.
func Search(s *store.Store, query string) []Offer {
	query = strings.ToLower(strings.TrimSpace(query))

	var out []Offer
	for _, event := range s.Query(nostr.Filter{Kinds: []nostr.Kind{Kind}}) {
		offer, err := Read(event)
		if err != nil {
			continue
		}
		haystack := strings.ToLower(strings.Join([]string{offer.ID, offer.Scheme, offer.Title, offer.Summary, offer.Name()}, " "))
		if query == "" || strings.Contains(haystack, query) {
			out = append(out, offer)
		}
	}

	sort.Slice(out, func(a, b int) bool {
		if out[a].Name() != out[b].Name() {
			return out[a].Name() < out[b].Name()
		}
		return out[a].ID < out[b].ID
	})
	return out
}

// Find returns one offer of a provider from a store. An empty id takes the
// provider's only offer, or fails when it has several.
func Find(s *store.Store, provider nostr.PubKey, id string) (Offer, error) {
	filter := nostr.Filter{Kinds: []nostr.Kind{Kind}, Authors: []nostr.PubKey{provider}}
	if id != "" {
		filter.Tags = nostr.TagMap{"d": {id}}
	}

	var offers []Offer
	for _, event := range s.Query(filter) {
		if offer, err := Read(event); err == nil {
			offers = append(offers, offer)
		}
	}
	switch {
	case len(offers) == 0:
		return Offer{}, errors.New("catalog: no announcement from that provider; run arcn discover")
	case len(offers) > 1 && id == "":
		return Offer{}, errors.New("catalog: that provider offers several capabilities; name one")
	}
	return offers[0], nil
}

// Install is one capability that the citizen trusts.
type Install struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
}

// Installs is the list of trusted capabilities, in one file.
type Installs struct {
	Path string
}

// List returns the installs.
func (i Installs) List() ([]Install, error) {
	body, err := os.ReadFile(i.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Install
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("catalog: %s is not a list of installs", i.Path)
	}
	return out, nil
}

// Add trusts one offer, or refreshes it when it is already there.
func (i Installs) Add(offer Offer) error {
	list, err := i.List()
	if err != nil {
		return err
	}
	entry := Install{Provider: offer.Provider.Hex(), ID: offer.ID, Name: offer.Name()}
	list = slices.DeleteFunc(list, func(e Install) bool { return e.Provider == entry.Provider && e.ID == entry.ID })
	list = append(list, entry)

	body, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(i.Path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(i.Path, body, 0o600)
}

// Trusted says whether the citizen installed a capability of a provider.
func (i Installs) Trusted(provider nostr.PubKey, id string) bool {
	list, _ := i.List()
	return slices.ContainsFunc(list, func(e Install) bool { return e.Provider == provider.Hex() && e.ID == id })
}

// Resolve reads a provider as 64 hex characters, or as the petname of an
// installed capability.
func (i Installs) Resolve(name string) (nostr.PubKey, string, error) {
	if key, err := nostr.PubKeyFromHex(name); err == nil {
		return key, "", nil
	}
	list, _ := i.List()
	for _, e := range list {
		if e.Name == name || e.ID == name {
			key, err := nostr.PubKeyFromHex(e.Provider)
			return key, e.ID, err
		}
	}
	return nostr.PubKey{}, "", fmt.Errorf("catalog: %q is not a key or an installed capability", name)
}
