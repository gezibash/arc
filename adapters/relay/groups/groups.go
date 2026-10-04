// Package groups makes a khatru relay host NIP-29 groups.
//
// A group lives on one relay, and the relay enforces it. An event of a
// group names the group with an h tag. The relay refuses an event for a
// group that does not exist, an event of a restricted group from a citizen
// who is not a member, and a moderation event from a citizen who is not an
// admin. It applies each moderation event it accepts, and signs the state of
// each group with its own key: metadata, kind 39000; admins, 39001; members,
// 39002.
//
// The operator makes each group. A citizen cannot make one.
package groups

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/eventstore"
	"fiatjaf.com/nostr/khatru"
	"fiatjaf.com/nostr/nip29"
)

// AdminRole is the role of an admin of a group that the operator makes.
const AdminRole = "admin"

// Groups is the group state of one relay.
type Groups struct {
	key   nostr.SecretKey
	relay *khatru.Relay
	db    eventstore.Store

	mu     sync.Mutex
	groups map[string]*nip29.Group
}

// Attach makes a relay host groups, and loads the groups that it signed
// before. Call it after the relay has its event store.
func Attach(rl *khatru.Relay, db eventstore.Store, key nostr.SecretKey) (*Groups, error) {
	g := &Groups{key: key, relay: rl, db: db, groups: map[string]*nip29.Group{}}
	pk := key.Public()

	for event := range db.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{nostr.KindSimpleGroupMetadata}, Authors: []nostr.PubKey{pk}}, 10000) {
		group, err := nip29.NewGroupFromMetadataEvent("", &event)
		if err != nil {
			continue
		}
		g.groups[group.Address.ID] = &group
	}
	for event := range db.QueryEvents(nostr.Filter{Kinds: []nostr.Kind{nostr.KindSimpleGroupAdmins, nostr.KindSimpleGroupMembers}, Authors: []nostr.PubKey{pk}}, 10000) {
		group := g.groups[event.Tags.GetD()]
		if group == nil {
			continue
		}
		if event.Kind == nostr.KindSimpleGroupMembers {
			_ = group.MergeInMembersEvent(&event)
		} else {
			_ = group.MergeInAdminsEvent(&event)
		}
	}

	rl.Info.SupportedNIPs = append(rl.Info.SupportedNIPs, 29)
	rl.Info.Self = &pk
	rl.OnEvent = g.check
	previous := rl.OnEventSaved
	rl.OnEventSaved = func(ctx context.Context, event nostr.Event) {
		if previous != nil {
			previous(ctx, event)
		}
		g.apply(ctx, event)
	}
	return g, nil
}

// Create makes a group, with its admins, when it does not exist. An open
// group takes posts from anyone; a restricted group only from its members.
func (g *Groups) Create(id, name string, restricted bool, admins []nostr.PubKey) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.groups[id]; ok {
		return nil
	}
	group, err := nip29.NewGroup("", id)
	if err != nil {
		return err
	}
	group.Name = name
	group.Restricted = restricted
	for _, admin := range admins {
		group.Members[admin] = []*nip29.Role{{Name: AdminRole}}
	}
	g.groups[id] = &group
	return g.publish(&group)
}

// publish signs and stores the state of a group.
func (g *Groups) publish(group *nip29.Group) error {
	now := nostr.Now()
	group.LastMetadataUpdate, group.LastAdminsUpdate, group.LastMembersUpdate = now, now, now
	for _, event := range []nostr.Event{group.ToMetadataEvent(), group.ToAdminsEvent(), group.ToMembersEvent()} {
		event.CreatedAt = now
		if err := event.Sign(g.key); err != nil {
			return err
		}
		if _, err := g.db.ReplaceEvent(event); err != nil {
			return err
		}
	}
	return nil
}

func isAdmin(group *nip29.Group, pk nostr.PubKey) bool {
	return len(group.Members[pk]) > 0
}

// check decides whether the relay takes an event.
func (g *Groups) check(_ context.Context, event nostr.Event) (bool, string) {
	if nip29.MetadataEventKinds.Includes(event.Kind) {
		return true, "blocked: only the relay signs the state of a group"
	}
	h := event.Tags.Find("h")
	moderation := nip29.ModerationEventKinds.Includes(event.Kind)
	if h == nil {
		if moderation || event.Kind == nostr.KindSimpleGroupJoinRequest || event.Kind == nostr.KindSimpleGroupLeaveRequest {
			return true, "invalid: a group event needs an h tag"
		}
		return false, ""
	}

	g.mu.Lock()
	defer g.mu.Unlock()
	group := g.groups[h[1]]
	if group == nil {
		return true, "blocked: this relay has no group " + h[1]
	}
	switch {
	case event.Kind == nostr.KindSimpleGroupCreateGroup:
		return true, "blocked: the group exists"
	case moderation:
		if !isAdmin(group, event.PubKey) {
			return true, "blocked: only an admin of the group can do this"
		}
		if _, err := nip29.PrepareModerationAction(event); err != nil {
			return true, "invalid: " + err.Error()
		}
	case event.Kind == nostr.KindSimpleGroupJoinRequest:
		if group.Closed {
			return true, "blocked: the group is closed"
		}
	case event.Kind == nostr.KindSimpleGroupLeaveRequest:
	default:
		if _, member := group.Members[event.PubKey]; group.Restricted && !member {
			return true, "restricted: only members of the group can post"
		}
	}
	return false, ""
}

// apply acts on an event that the relay stored: a moderation event changes
// the group, and a join or leave request changes its members.
func (g *Groups) apply(ctx context.Context, event nostr.Event) {
	h := event.Tags.Find("h")
	if h == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	group := g.groups[h[1]]
	if group == nil {
		return
	}

	switch {
	case event.Kind == nostr.KindSimpleGroupJoinRequest:
		if _, ok := group.Members[event.PubKey]; !ok {
			group.Members[event.PubKey] = nil
		}
	case event.Kind == nostr.KindSimpleGroupLeaveRequest:
		if isAdmin(group, event.PubKey) {
			return
		}
		delete(group.Members, event.PubKey)
	case nip29.ModerationEventKinds.Includes(event.Kind):
		action, err := nip29.PrepareModerationAction(event)
		if err != nil {
			return
		}
		if remove, ok := action.(nip29.DeleteEvent); ok {
			g.remove(ctx, group.Address.ID, remove.Targets)
			return
		}
		action.Apply(group)
	default:
		return
	}
	if err := g.publish(group); err != nil {
		g.relay.Log.Printf("groups: %v", err)
	}
}

// remove deletes events of one group. It leaves an event of another group,
// or of no group, where it is.
func (g *Groups) remove(ctx context.Context, id string, targets []nostr.ID) {
	// Collect first: a store such as bolt cannot delete while a query of it
	// is open.
	var doomed []nostr.ID
	for event := range g.db.QueryEvents(nostr.Filter{IDs: targets}, len(targets)) {
		if h := event.Tags.Find("h"); h != nil && h[1] == id {
			doomed = append(doomed, event.ID)
		}
	}
	for _, target := range doomed {
		if err := g.relay.DeleteEvent(ctx, target); err != nil {
			g.relay.Log.Printf("groups: %v", err)
		}
	}
}

// Admins lists the admins of a group.
func (g *Groups) Admins(id string) ([]nostr.PubKey, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	group := g.groups[id]
	if group == nil {
		return nil, fmt.Errorf("groups: no group %s", id)
	}
	var out []nostr.PubKey
	for pk := range group.Members {
		if isAdmin(group, pk) {
			out = append(out, pk)
		}
	}
	slices.SortFunc(out, func(a, b nostr.PubKey) int { return slices.Compare(a[:], b[:]) })
	return out, nil
}
