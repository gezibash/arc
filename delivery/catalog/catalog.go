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
	"github.com/gezibash/arc/iface"
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
	// Manifest is the manifest of interface version 1, when the
	// announcement holds one. A manifest of the older stack leaves it nil.
	Manifest *iface.Manifest
	Event    nostr.Event
}

// Name is the petname of the provider.
func (o Offer) Name() string { return identity.Name(o.Provider[:]) }

// Announce makes the announcement of one capability package. The package is
// the normalized manifest; its capability id becomes the d tag.
func Announce(k keys.Signer, pkg map[string]any, at nostr.Timestamp) (nostr.Event, error) {
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
	if err := k.Sign(&event); err != nil {
		return nostr.Event{}, err
	}
	return event, nil
}

// AnnounceManifest makes the announcement of one manifest of interface
// version 1. Its id becomes the d tag.
func AnnounceManifest(k keys.Signer, data []byte, at nostr.Timestamp) (nostr.Event, error) {
	m, err := iface.Parse(data)
	if err != nil {
		return nostr.Event{}, err
	}
	body, err := json.Marshal(m)
	if err != nil {
		return nostr.Event{}, err
	}
	tags := nostr.Tags{{"d", m.ID}, {"t", m.ID}, {"t", m.Shape}}
	event := nostr.Event{Kind: Kind, CreatedAt: at, Tags: tags, Content: string(body)}
	if err := k.Sign(&event); err != nil {
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
	if _, ok := pkg["interface"]; ok {
		return readManifest(event, pkg)
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

// readManifest opens an announcement of interface version 1.
func readManifest(event nostr.Event, pkg map[string]any) (Offer, error) {
	m, err := iface.Parse([]byte(event.Content))
	if err != nil {
		return Offer{}, fmt.Errorf("catalog: %w", err)
	}
	if m.ID != event.Tags.GetD() {
		return Offer{}, errors.New("catalog: the announcement names another capability than its manifest")
	}
	offer := Offer{
		Provider: event.PubKey, ID: m.ID, Package: pkg, Manifest: m, Event: event,
		Title: m.Title, Summary: m.Summary, Scheme: m.ID, Path: "/",
	}
	if m.Service != nil {
		offer.Method = strings.ToUpper(m.Service.Method)
		if m.Service.Path != "" {
			offer.Path = m.Service.Path
		}
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
	// As is the name that runs a capability of interface version 1.
	As string `json:"as,omitempty"`
	// Consent is what the citizen agreed to at install: the kinds and the
	// group relay of the manifest.
	Consent *iface.Consent `json:"consent,omitempty"`
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

// Add trusts one offer, or refreshes it when it is already there. A
// capability of interface version 1 runs as the name as, which no other
// install can hold.
func (i Installs) Add(offer Offer, as string) error {
	list, err := i.List()
	if err != nil {
		return err
	}
	entry := Install{Provider: offer.Provider.Hex(), ID: offer.ID, Name: offer.Name(), As: as}
	if offer.Manifest != nil {
		consent := iface.ConsentOf(offer.Manifest)
		entry.Consent = &consent
	}
	for _, e := range list {
		if as != "" && e.As == as && (e.Provider != entry.Provider || e.ID != entry.ID) {
			return fmt.Errorf("catalog: %s already runs a capability of %s; choose another name with --as", as, e.Name)
		}
	}
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

// Remove takes one install out, by the name that runs it, or by the petname
// of its provider for an install that runs through arcn call. It returns the
// install that it removed.
func (i Installs) Remove(name string) (Install, error) {
	list, err := i.List()
	if err != nil {
		return Install{}, err
	}
	var matches []Install
	for _, e := range list {
		if e.As == name || (e.As == "" && e.Name == name) {
			matches = append(matches, e)
		}
	}
	switch len(matches) {
	case 0:
		return Install{}, fmt.Errorf("catalog: nothing installed as %q", name)
	case 1:
	default:
		return Install{}, fmt.Errorf("catalog: %q names %d installs; remove the others by their names first", name, len(matches))
	}
	gone := matches[0]
	list = slices.DeleteFunc(list, func(e Install) bool { return e.Provider == gone.Provider && e.ID == gone.ID })

	body, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return Install{}, err
	}
	return gone, os.WriteFile(i.Path, body, 0o600)
}

// Trusted says whether the citizen installed a capability of a provider.
func (i Installs) Trusted(provider nostr.PubKey, id string) bool {
	list, _ := i.List()
	return slices.ContainsFunc(list, func(e Install) bool { return e.Provider == provider.Hex() && e.ID == id })
}

// Named returns the install that runs as a name.
func (i Installs) Named(as string) (Install, bool) {
	list, _ := i.List()
	for _, e := range list {
		if e.As == as {
			return e, true
		}
	}
	return Install{}, false
}

// Resolve reads a provider as 64 hex characters, or as the petname of an
// installed capability.
func (i Installs) Resolve(name string) (nostr.PubKey, string, error) {
	if key, err := nostr.PubKeyFromHex(name); err == nil {
		return key, "", nil
	}
	list, _ := i.List()
	for _, e := range list {
		if e.As == name || e.Name == name || e.ID == name {
			key, err := nostr.PubKeyFromHex(e.Provider)
			return key, e.ID, err
		}
	}
	return nostr.PubKey{}, "", fmt.Errorf("catalog: %q is not a key or an installed capability", name)
}
