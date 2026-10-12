package citizen

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/core/store"
	"github.com/gezibash/arc/runtime/catalog"
)

// ImportPages seals each draft that the citizen key sealed to itself again,
// in the format of the data roots, so that a device reads it with no
// signer. It opens each old draft through the signer once. The new wrap is
// one second newer than the old one, so the store keeps only the new one,
// and a second run finds nothing to do. The app of a draft is the first
// install that names the kind inside as sealed. It returns the count of
// drafts that it sealed again.
func (sess *Session) ImportPages(ctx context.Context, installs catalog.Installs) (int, error) {
	wraps, err := sess.Node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{draft.Kind}, Authors: []nostr.PubKey{sess.Key.Public}})
	if err != nil {
		return 0, err
	}
	var old []nostr.Event
	for _, wrap := range wraps {
		if _, ok := draft.Gen(wrap); !ok && wrap.Content != "" && !strings.HasPrefix(wrap.Tags.GetD(), "arc-") {
			old = append(old, wrap)
		}
	}
	if len(old) == 0 {
		return 0, nil
	}
	apps, err := sealedApps(installs)
	if err != nil {
		return 0, err
	}
	root, err := sess.DataRoot(ctx)
	if err != nil {
		return 0, err
	}
	done := 0
	var problems []error
	for _, wrap := range old {
		k := wrap.Tags.Find("k")
		if k == nil || apps[k[1]] == "" {
			continue
		}
		if err := sess.importPage(ctx, root, apps[k[1]], wrap); err != nil {
			problems = append(problems, fmt.Errorf("draft %s: %w", wrap.ID.Hex()[:8], err))
			continue
		}
		done++
	}
	return done, errors.Join(problems...)
}

// sealedApps maps each sealed kind, as a number, to the first install that
// names it.
func sealedApps(installs catalog.Installs) (map[string]string, error) {
	list, err := installs.List()
	if err != nil {
		return nil, err
	}
	apps := map[string]string{}
	for _, install := range list {
		if install.Consent == nil {
			continue
		}
		for kind, visibility := range install.Consent.Kinds {
			if visibility == "sealed" && apps[kind] == "" {
				apps[kind] = install.ID
			}
		}
	}
	return apps, nil
}

func (sess *Session) importPage(ctx context.Context, root devices.Root, app string, wrap nostr.Event) error {
	opened, err := draft.Open(ctx, sess.Signer, wrap)
	if err != nil {
		return err
	}
	key, err := devices.ContentKey(root.Key, app)
	if err != nil {
		return err
	}
	s := draft.Sealer{Citizen: sess.Key.Public, Signer: sess.PageSigner(), Gen: root.Gen, Key: key}
	at := wrap.CreatedAt + 1
	inner := opened.Event
	var events []nostr.Event
	if ids := opened.Parts(); len(ids) > 0 {
		texts, err := sess.oldParts(ctx, ids)
		if err != nil {
			return err
		}
		tag := nostr.Tag{"parts"}
		for _, text := range texts {
			part, err := s.Part(ctx, opened.D, text, at)
			if err != nil {
				return err
			}
			events = append(events, part)
			tag = append(tag, part.ID.Hex())
		}
		tags := nostr.Tags{}
		for _, t := range inner.Tags {
			if t[0] != "parts" {
				tags = append(tags, t)
			}
		}
		inner.Tags = append(tags, tag)
	}
	next, err := s.Wrap(ctx, opened.D, inner, at)
	if err != nil {
		return err
	}
	for _, event := range append(events, next) {
		result, err := sess.Node.Store.Save(event)
		if err != nil {
			return err
		}
		if result.Outcome == store.Refused {
			return fmt.Errorf("the store refused the new page: %s", result.Reason)
		}
	}
	return nil
}

// oldParts opens the parts of an old draft, in order, through the signer.
func (sess *Session) oldParts(ctx context.Context, ids []string) ([]string, error) {
	want := make([]nostr.ID, 0, len(ids))
	for _, id := range ids {
		parsed, err := nostr.IDFromHex(id)
		if err != nil {
			return nil, fmt.Errorf("the parts tag names %q, which is not an event", id)
		}
		want = append(want, parsed)
	}
	events, err := sess.Node.Store.Query(nostr.Filter{IDs: want, Kinds: []nostr.Kind{draft.PartKind}, Authors: []nostr.PubKey{sess.Key.Public}})
	if err != nil {
		return nil, err
	}
	byID := map[nostr.ID]nostr.Event{}
	for _, event := range events {
		byID[event.ID] = event
	}
	out := make([]string, 0, len(want))
	for i, id := range want {
		event, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("part %d of %d is missing: sync first", i+1, len(want))
		}
		text, err := draft.OpenPart(ctx, sess.Signer, event)
		if err != nil {
			return nil, fmt.Errorf("part %d of %d: %w", i+1, len(want), err)
		}
		out = append(out, text)
	}
	return out, nil
}
