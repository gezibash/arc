package iface

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
	"github.com/gezibash/arc/runtime/iface/manifest"
)

func (r *run) publish(p *manifest.Publish) ([]*entry, error) {
	switch r.kindOf(p.Kind).Visibility {
	case "sealed":
		return r.publishSealed(p)
	case "private":
		return r.publishPrivate(p)
	default:
		return r.publishPlain(p)
	}
}

// tagsOf renders the tags of a publish, and leaves out a tag whose value is
// empty.
func (r *run) tagsOf(p *manifest.Publish) (nostr.Tags, error) {
	tags := nostr.Tags{}
	for _, tag := range p.Tags {
		rendered := nostr.Tag{tag[0]}
		for _, value := range tag[1:] {
			text, err := r.template(value)
			if err != nil {
				return nil, err
			}
			rendered = append(rendered, text)
		}
		if len(rendered) > 1 && rendered[1] != "" {
			tags = append(tags, rendered)
		}
	}
	return tags, nil
}

// publishPrivate seals the event to each recipient, through the mail layer.
func (r *run) publishPrivate(p *manifest.Publish) ([]*entry, error) {
	content, err := r.content(p.Content)
	if err != nil {
		return nil, err
	}
	tags, err := r.tagsOf(p)
	if err != nil {
		return nil, err
	}
	tags, err = r.enforceFrontmatter(p, content, tags)
	if err != nil {
		return nil, err
	}
	var pks []nostr.PubKey
	var names, recipients []string
	for _, text := range p.To {
		to, err := r.template(text)
		if err != nil {
			return nil, err
		}
		pk, err := nostr.PubKeyFromHex(to)
		if err != nil {
			return nil, errors.New("a recipient must come from a key argument")
		}
		pks = append(pks, pk)
		names = append(names, r.env.Name(pk))
		recipients = append(recipients, pk.Hex())
	}
	kind := nostr.Kind(r.kindOf(p.Kind).Kind)
	if r.flags.DryRun {
		rumor := nostr.Event{Kind: kind, CreatedAt: nostr.Now(), Content: content, Tags: tags, PubKey: r.env.Me()}
		return nil, r.dryRun(map[string]any{"to": recipients[0], "recipients": recipients, "sealed": true, "event": unsignedOf(rumor)})
	}
	if err := r.env.SendPrivate(r.ctx, pks, kind, content, tags); err != nil {
		return nil, err
	}
	fmt.Fprintf(r.stdio.Err, "sealed for %s; it travels with the next sync if no relay took it\n", strings.Join(names, ", "))
	return nil, nil
}

// publishPlain signs a public event, or an event of the group, and sends it.
func (r *run) publishPlain(p *manifest.Publish) ([]*entry, error) {
	content, err := r.content(p.Content)
	if err != nil {
		return nil, err
	}
	tags, err := r.tagsOf(p)
	if err != nil {
		return nil, err
	}
	tags, err = r.enforceFrontmatter(p, content, tags)
	if err != nil {
		return nil, err
	}
	var relays []string
	if r.kindOf(p.Kind).Visibility == "group" {
		// NIP-29: the h tag names the group, and the NIP-70 tag keeps the
		// event on the group's relay.
		g := r.in.Manifest.Group
		tags = append(nostr.Tags{{"h", g.ID}, {"-"}}, tags...)
		relays = []string{g.Relay}
	}
	if p.D != "" {
		d, err := r.template(p.D)
		if err != nil {
			return nil, err
		}
		tags = append(nostr.Tags{{"d", d}}, tags...)
	}
	event := nostr.Event{Kind: nostr.Kind(r.kindOf(p.Kind).Kind), CreatedAt: nostr.Now(), Content: content, Tags: tags, PubKey: r.env.Me()}
	if r.flags.DryRun {
		return nil, r.dryRun(map[string]any{"relays": relays, "event": unsignedOf(event)})
	}
	if err := r.env.Keyer().SignEvent(r.ctx, &event); err != nil {
		return nil, err
	}
	if err := r.env.Publish(r.ctx, []nostr.Event{event}, relays); err != nil {
		return nil, err
	}
	fmt.Fprintln(r.stdio.Err, nip19.EncodeNevent(event.ID, relays, event.PubKey)) //arc:raw-ok a nevent in bech32
	return []*entry{plainEntry(event, p.Kind)}, nil
}

// unsignedOf shows an event that nobody has signed.
func unsignedOf(e nostr.Event) map[string]any {
	return map[string]any{"pubkey": e.PubKey.Hex(), "created_at": e.CreatedAt, "kind": e.Kind, "tags": e.Tags, "content": e.Content}
}

// dryRun writes what a command would sign, and signs nothing.
func (r *run) dryRun(what map[string]any) error {
	encoder := json.NewEncoder(r.stdio.Out)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	fmt.Fprintln(r.stdio.Err, "dry run: nothing is signed or sent")
	return encoder.Encode(what)
}
