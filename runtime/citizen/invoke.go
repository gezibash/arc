package citizen

import (
	"context"
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/call"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/catalog"
	"github.com/gezibash/arc/runtime/iface"
)

func (sess *Session) FindOffer(ctx context.Context, provider nostr.PubKey, id string) (catalog.Offer, error) {
	offer, err := catalog.Find(sess.Node.Store, provider, id)
	if err == nil {
		return offer, nil
	}
	reports, errs := sess.Node.Pull(ctx, nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}}, sess.Relays)
	offer, err = catalog.Find(sess.Node.Store, provider, id)
	if unreached := node.Unreached(reports, errs); err != nil && unreached != nil {
		return offer, fmt.Errorf("this machine holds no announcement from that provider, and %w", unreached)
	}
	return offer, err
}

// newestAnnouncements asks the relays for the announcements of a provider,
// so that a call meets a new manifest at once. If no relay answers, the call
// uses the announcements that this machine holds.
func newestAnnouncements(ctx context.Context, sess *Session, provider nostr.PubKey) {
	if len(sess.Relays) == 0 {
		return
	}
	filter := nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}}
	if err := node.Unreached(sess.Node.Pull(ctx, filter, sess.Relays)); err != nil {
		sess.warnf("%v\nthis uses the announcement that this machine holds\n", err)
	}
}

// callTarget finds the capability that a call names: by an address,
// <scheme>+arc://<provider>/<path>, or by a provider and a capability id in
// flag. It returns the path of the address, or "" for a provider.
func (sess *Session) Target(ctx context.Context, installs catalog.Installs, target, flag string) (nostr.PubKey, catalog.Offer, string, error) {
	if !catalog.IsAddress(target) {
		provider, id, err := installs.Resolve(ctx, target)
		if err != nil {
			return provider, catalog.Offer{}, "", err
		}
		if flag != "" {
			id = flag
		}
		newestAnnouncements(ctx, sess, provider)
		offer, err := sess.FindOffer(ctx, provider, id)
		return provider, offer, "", err
	}

	address, err := catalog.ParseAddress(target)
	if err != nil {
		return nostr.PubKey{}, catalog.Offer{}, "", err
	}
	provider, _, err := installs.Resolve(ctx, address.Provider)
	if err != nil {
		return provider, catalog.Offer{}, "", err
	}
	newestAnnouncements(ctx, sess, provider)
	if flag != "" {
		offer, err := sess.FindOffer(ctx, provider, flag)
		if err == nil && offer.Scheme != address.Scheme {
			err = fmt.Errorf("the capability %s has the scheme %s, and the address names %s", offer.ID, offer.Scheme, address.Scheme)
		}
		return provider, offer, address.Path, err
	}
	offer, err := catalog.FindScheme(sess.Node.Store, provider, address.Scheme)
	if err != nil {
		// Ask the relays for the announcements of the provider, then look
		// again.
		reports, errs := sess.Node.Pull(ctx, nostr.Filter{Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}}, sess.Relays)
		offer, err = catalog.FindScheme(sess.Node.Store, provider, address.Scheme)
		if unreached := node.Unreached(reports, errs); err != nil && unreached != nil {
			err = fmt.Errorf("this machine holds no announcement from that provider, and %w", unreached)
		}
	}
	return provider, offer, address.Path, err
}

// callAddress makes one live call, as this citizen, to an installed capability
// that an address names. A provider program that this citizen serves calls
// this way, so it can do only what `arc call` can do for this citizen.
func (sess *Session) CallAddress(ctx context.Context, installs catalog.Installs, address, body string) (call.Reply, error) {
	provider, offer, path, err := sess.installedService(ctx, installs, address, session.RequestReply)
	if err != nil {
		return call.Reply{}, err
	}
	request := call.Request{Capability: offer.ID, Method: offer.Method, Path: path, Body: body}
	reply, _, _, err := sess.LiveCall(ctx, provider, request, call.CallTimeout)
	return reply, err
}

// Installed resolves the latest manifest and enforces the recorded consent.
func (sess *Session) Installed(ctx context.Context, installs catalog.Installs, name string) (iface.Installed, error) {
	install, ok, err := installs.Named(name)
	if err != nil {
		return iface.Installed{}, err
	}
	if !ok {
		return iface.Installed{}, fmt.Errorf("unknown command %q: see arc --help, or install a capability with arc install", name)
	}
	provider, err := nostr.PubKeyFromHex(install.Provider)
	if err != nil {
		return iface.Installed{}, err
	}

	// Ask for the newest announcement first, so a new version meets the
	// consent check at once.
	if err := node.Unreached(sess.Node.Pull(ctx, nostr.Filter{
		Kinds: []nostr.Kind{catalog.Kind}, Authors: []nostr.PubKey{provider}, Tags: nostr.TagMap{"d": {install.ID}},
	}, sess.Relays)); err != nil {
		sess.warnf("%v\nthis uses the announcement that this machine holds\n", err)
	}
	offer, err := sess.FindOffer(ctx, provider, install.ID)
	if err != nil {
		return iface.Installed{}, err
	}
	if offer.Manifest == nil {
		return iface.Installed{}, fmt.Errorf("%s predates interface version 1: call it with arc call %s", name, offer.Name())
	}
	// A new version of the manifest can do no more than the citizen agreed
	// to, until they agree again.
	var changes []string
	if install.Consent == nil {
		changes = []string{"this install records no consent"}
	} else {
		changes = install.Consent.Changes(offer.Manifest)
	}
	if len(changes) > 0 {
		return iface.Installed{}, fmt.Errorf("the author changed what %s can do:\n  %s\nto agree, install it again: arc install %s %s --as %s",
			name, strings.Join(changes, "\n  "), install.Provider, install.ID, name)
	}

	return iface.Installed{Manifest: offer.Manifest, Author: provider, Name: name}, nil
}

// installedService is the consent and interaction check shared by all consumers.
func (sess *Session) installedService(ctx context.Context, installs catalog.Installs, address string, mode session.Mode) (nostr.PubKey, catalog.Offer, string, error) {
	if !catalog.IsAddress(address) {
		return nostr.PubKey{}, catalog.Offer{}, "", fmt.Errorf("%q is not an address: <scheme>+arc://<provider>/<path>", address)
	}
	public, offer, path, err := sess.Target(ctx, installs, address, "")
	if err != nil {
		return public, offer, path, err
	}
	trusted, err := installs.Trusted(public, offer.ID)
	if err != nil {
		return public, offer, path, err
	}
	if !trusted {
		return public, offer, path, fmt.Errorf("not_installed: the citizen must install it: arc install %s %s", public.Hex(), offer.ID)
	}
	if !offer.SupportsInteraction(mode) {
		return public, offer, path, fmt.Errorf("%s does not declare %s: %w", offer.ID, mode, session.ErrUnsupported)
	}
	return public, offer, path, nil
}
