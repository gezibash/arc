package catalog

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/store"
)

// Address is a capability address: <scheme>+arc://<provider>/<path>. See
// docs/interface/SPEC.md, section 14.1.
type Address struct {
	// Scheme names the capability: its id, or the scheme of its manifest.
	Scheme string
	// Provider is the text of the provider: a key, an npub, an installed
	// name, or a domain for NIP-05. The caller resolves it.
	Provider string
	// Path is the resource that the request names. It is "/" when the
	// address names none.
	Path string
}

var (
	schemePattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	providerPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	pathPattern     = regexp.MustCompile(`^/[A-Za-z0-9/._~-]*$`)
)

// IsAddress says whether text is written as a capability address.
func IsAddress(text string) bool {
	scheme, _, ok := strings.Cut(text, "+arc://")
	return ok && !strings.ContainsAny(scheme, "/:")
}

// ParseAddress reads a capability address. It refuses user information, a
// port, a query, a fragment, a percent escape, and a dot segment, so that an
// address names one resource and nothing else.
func ParseAddress(text string) (Address, error) {
	scheme, rest, ok := strings.Cut(text, "+arc://")
	if !ok {
		return Address{}, errors.New("an address has the form <scheme>+arc://<provider>/<path>")
	}
	if !schemePattern.MatchString(scheme) {
		return Address{}, fmt.Errorf("the scheme %q must be lower-case letters, digits and hyphens", scheme)
	}
	provider, path, _ := strings.Cut(rest, "/")
	path = "/" + path
	switch {
	case strings.ContainsAny(text, "?#"):
		return Address{}, errors.New("an address has no query and no fragment")
	case strings.Contains(provider, "@"):
		return Address{}, errors.New("an address has no user information: name the provider by its key or its npub")
	case strings.Contains(provider, ":"):
		return Address{}, errors.New("an address has no port: the provider is a key, not a host")
	case !providerPattern.MatchString(provider):
		return Address{}, fmt.Errorf("%q is not a provider: use its key, its npub, or an installed name", provider)
	case strings.Contains(path, "%"):
		return Address{}, errors.New("an address has no percent escape")
	case !pathPattern.MatchString(path):
		return Address{}, fmt.Errorf("the path %q holds a character that an address does not allow", path)
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return Address{}, errors.New("an address has no dot segment")
		}
	}
	return Address{Scheme: scheme, Provider: provider, Path: path}, nil
}

// FindScheme finds the capability of a provider that an address names: the
// one whose id is the scheme, or else the only one whose manifest has that
// scheme.
func FindScheme(s *store.Store, provider nostr.PubKey, scheme string) (Offer, error) {
	var matches []Offer
	for _, event := range s.Query(nostr.Filter{Kinds: []nostr.Kind{Kind}, Authors: []nostr.PubKey{provider}}) {
		offer, err := Read(event)
		if err != nil {
			continue
		}
		if offer.ID == scheme {
			return offer, nil
		}
		if offer.Scheme == scheme {
			matches = append(matches, offer)
		}
	}
	switch len(matches) {
	case 0:
		return Offer{}, fmt.Errorf("catalog: that provider offers no capability %q; run arc info %s", scheme, provider.Hex())
	case 1:
		return matches[0], nil
	}
	ids := make([]string, len(matches))
	for i, m := range matches {
		ids[i] = m.ID
	}
	return Offer{}, fmt.Errorf("catalog: several capabilities of that provider have the scheme %q: %s; name one with --capability", scheme, strings.Join(ids, ", "))
}
