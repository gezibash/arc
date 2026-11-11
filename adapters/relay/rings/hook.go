package rings

import (
	"context"
	"net/url"
	"slices"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/core/private"
)

// hook adds the NIP-11 tag, and watches the wraps and the reads. It sends
// a NIP-42 challenge on each new connection, so that a citizen can
// authenticate before it reads its mail: khatru sends one only when a
// query needs it, and a wrap needs none.
func (r *Rings) hook(rl *khatru.Relay) {
	rl.Info.Tags = append(rl.Info.Tags, Tag)

	connect := rl.OnConnect
	rl.OnConnect = func(ctx context.Context) {
		khatru.RequestAuth(ctx)
		if connect != nil {
			connect(ctx)
		}
	}

	saved := rl.OnEventSaved
	rl.OnEventSaved = func(ctx context.Context, event nostr.Event) {
		if event.Kind == private.WrapKind {
			r.wrap(event)
		}
		if saved != nil {
			saved(ctx, event)
		}
	}
	added := rl.OnListenerAdded
	rl.OnListenerAdded = func(ws *khatru.WebSocket, ssid int, id string, filter nostr.Filter) {
		r.listen(ws, ssid, filter)
		if added != nil {
			added(ws, ssid, id, filter)
		}
	}
	disconnect := rl.OnDisconnect
	rl.OnDisconnect = func(ctx context.Context) {
		if ws := khatru.GetConnection(ctx); ws != nil {
			r.mu.Lock()
			delete(r.told, ws)
			r.mu.Unlock()
		}
		if disconnect != nil {
			disconnect(ctx)
		}
	}
	removed := rl.OnListenerRemoved
	rl.OnListenerRemoved = func(ws *khatru.WebSocket, ssid int, id string, filter nostr.Filter) {
		r.unlisten(ws, ssid)
		if removed != nil {
			removed(ws, ssid, id, filter)
		}
	}
}

// wrap notes a relay-form wrap, and rings at once when a ring is due. A
// courier-form wrap has no p tag, and a live wrap is ephemeral and never
// saved, so neither rings. The relay counts the subscriptions of a citizen
// when they open, so it knows before the broadcast whether the citizen
// reads the wrap live.
func (r *Rings) wrap(event nostr.Event) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for tag := range event.Tags.FindAll("p") {
		pk, err := nostr.PubKeyFromHex(tag[1])
		if err != nil {
			continue
		}
		s := r.by[pk]
		if s == nil {
			// Most wraps are for citizens with no registration, so this
			// line is for debugging only.
			r.cfg.Logger.Debug("no ring", "citizen", short(pk), "reason", whyNoRegistration)
			continue
		}
		if r.watching[pk] > 0 {
			// The open subscription reads the wrap: no ring waits, so no
			// try can start.
			s.wrap(now, true, event.ID.Hex())
			s.renew(now, r.cfg.TTL)
			r.save(pk, s)
			r.stop(pk)
			r.cfg.Logger.Info("no ring", "citizen", short(pk), "reason", whyWatching)
			continue
		}
		s.wrap(now, false, event.ID.Hex())
		// A ring that starts now saves the state, with this wrap in it,
		// before it calls: one save on the way to the ring, not two. Each
		// save syncs the disk, and a busy disk made a ring late (#451).
		busy := r.busy[pk]
		why, wait := r.schedule(pk)
		if busy || !r.busy[pk] {
			r.save(pk, s)
		}
		switch {
		case why == "":
		case wait > 0:
			r.cfg.Logger.Info("no ring", "citizen", short(pk), "reason", why, "wait", wait.Round(time.Second))
		default:
			r.cfg.Logger.Info("no ring", "citizen", short(pk), "reason", why)
		}
	}
}

// listen notes a subscription. A subscription for the wraps of a citizen,
// on a connection that reads for the citizen, is a read, and it reads each
// wrap that arrives while it is open.
func (r *Rings) listen(ws *khatru.WebSocket, ssid int, filter nostr.Filter) {
	if len(filter.Kinds) > 0 && !slices.Contains(filter.Kinds, private.WrapKind) {
		return
	}
	readers := r.cfg.Readers(ws)
	var citizens []nostr.PubKey
	for _, value := range filter.Tags["p"] {
		pk, err := nostr.PubKeyFromHex(value)
		if err == nil && slices.Contains(readers, pk) && !slices.Contains(citizens, pk) {
			citizens = append(citizens, pk)
		}
	}
	if len(citizens) == 0 {
		return
	}
	now := r.cfg.Now()
	var notices []string
	r.mu.Lock()
	if r.subs[ws] == nil {
		r.subs[ws] = map[int][]nostr.PubKey{}
	}
	r.subs[ws][ssid] = citizens
	for _, pk := range citizens {
		r.watching[pk]++
		if s := r.by[pk]; s != nil {
			s.read(now)
			s.renew(now, r.cfg.TTL)
			r.save(pk, s)
			r.stop(pk)
			host := s.URL
			if u, err := url.Parse(s.URL); err == nil {
				host = u.Host
			}
			if text := r.notice(ws, noticeKey{pk, "failure"}, s.failure(host)); text != "" {
				notices = append(notices, text)
			}
			if text := r.notice(ws, noticeKey{pk, "cap"}, s.low(now)); text != "" {
				notices = append(notices, text)
			}
		}
	}
	r.mu.Unlock()
	// A slow connection must not hold the lock of every ring.
	for _, text := range notices {
		// A closed connection loses the notice; the next read gets it.
		_ = ws.WriteJSON(nostr.NoticeEnvelope(text))
	}
}

// noticeKey names one notice of one citizen.
type noticeKey struct {
	citizen nostr.PubKey
	notice  string
}

// notice is text, as a NOTICE for a connection that reads for a citizen,
// once for each connection and kind of notice, or empty. Only the citizen,
// or its device, reads it. The caller holds r.mu.
func (r *Rings) notice(ws *khatru.WebSocket, which noticeKey, text string) string {
	if text == "" || r.told[ws][which] {
		return ""
	}
	if r.told[ws] == nil {
		r.told[ws] = map[noticeKey]bool{}
	}
	r.told[ws][which] = true
	return text
}

func (r *Rings) unlisten(ws *khatru.WebSocket, ssid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	citizens, ok := r.subs[ws][ssid]
	if !ok {
		return
	}
	delete(r.subs[ws], ssid)
	if len(r.subs[ws]) == 0 {
		delete(r.subs, ws)
		// khatru removes the listeners of a closed connection after
		// OnDisconnect, and a REQ that raced the close can add one in
		// between: free its told set here too.
		if ws.Context.Err() != nil {
			delete(r.told, ws)
		}
	}
	for _, pk := range citizens {
		if r.watching[pk]--; r.watching[pk] <= 0 {
			delete(r.watching, pk)
		}
	}
}
