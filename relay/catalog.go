package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/identity"
)

// Each relay keeps a catalog: the signed announcements that its partners
// hold, and the way to reach each publisher.
//
// A partner is asked for its catalog every two seconds. The answer is a page
// of signed records with the path to each publisher. Nothing enters the
// catalog unsigned, and nothing leaves this relay that its publisher did not
// share.
//
// A route begins at the partner that sent it and ends at the relay that the
// publisher signed as its home. It never repeats a relay, never names this
// relay, and never crosses more than eight connections.
const (
	// CatalogPoll is how often a partner is asked.
	CatalogPoll = 2 * time.Second
	// CatalogLife is how long the view of a partner stands without a
	// refresh.
	CatalogLife = 30 * time.Second
	// MaxImported is how many records the catalog holds.
	MaxImported = 10_000
	// MaxCatalogPage is how many records one page carries.
	MaxCatalogPage = 50
	// MaxCatalogBytes caps one page.
	MaxCatalogBytes = 220 * 1024
	// MaxRouteHops is how many connections a route may cross.
	MaxRouteHops = 8
	// ConversationLife is how long a relay lets the answer of a request
	// come back.
	ConversationLife = 180 * time.Second
)

// imported is one record that a partner gave us.
type imported struct {
	entry     *announce.Entry
	peer      []byte
	path      [][]byte
	expiresAt time.Time
}

// peerView is what we know of one partner.
type peerView struct {
	synced    bool
	truncated bool
	refreshed time.Time
	// token, epoch and after carry a snapshot that runs over several pages.
	token string
	epoch string
	after string
	page  map[string]*imported
}

type catalog struct {
	relay *Relay
	epoch string

	mu      sync.Mutex
	imports map[string]map[string]*imported
	views   map[string]*peerView
	// live holds the paths that a live lookup found, by public key. They
	// serve the router until their record expires, and go with the link of
	// the partner that gave them.
	live map[string]*imported
}

func newCatalog(r *Relay) *catalog {
	seed := make([]byte, 16)
	rand.Read(seed)

	return &catalog{
		relay:   r,
		epoch:   hex.EncodeToString(seed),
		imports: map[string]map[string]*imported{},
		views:   map[string]*peerView{},
		live:    map[string]*imported{},
	}
}

// peerUp reads the catalog of a partner from the start, and keeps it fresh.
func (c *catalog) peerUp(peer []byte) {
	c.mu.Lock()
	c.views[string(peer)] = &peerView{}
	c.mu.Unlock()

	go c.follow(peer)
}

// forget drops the view of a partner that left, and the paths of lookups
// that it answered.
func (c *catalog) forget(peer []byte) {
	c.mu.Lock()
	delete(c.imports, string(peer))
	delete(c.views, string(peer))
	for key, held := range c.live {
		if string(held.peer) == string(peer) {
			delete(c.live, key)
		}
	}
	c.mu.Unlock()
}

// install keeps the paths that a live lookup found, so that a call to one of
// its publishers finds the way. A path of a partner whose link ended by now
// is not kept.
func (c *catalog) install(result outcome) {
	now := time.Now()

	// The links are read before the catalog locks, so the two locks never
	// wait for one another.
	ready := map[string]bool{}
	for _, found := range result.hits {
		if found.peer != nil {
			_, err := c.relay.federation.readyLink(found.peer)
			ready[string(found.peer)] = err == nil
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for key, found := range result.hits {
		if found.peer == nil || !ready[string(found.peer)] {
			continue
		}

		expires := time.Unix(found.entry.ExpiresAt, 0)
		if limit := now.Add(CatalogLife); expires.After(limit) {
			expires = limit
		}
		c.live[key] = &imported{entry: found.entry, peer: found.peer, path: found.path, expiresAt: expires}
	}
}

// cold says whether the catalog cannot answer a search alone: a partner that
// the operator approved has no view here, or a view is not complete.
func (c *catalog) cold(approved int) bool {
	c.mu.Lock()
	views := len(c.views)
	c.mu.Unlock()
	return views < approved || c.partial()
}

// follow asks one partner until its link ends.
func (c *catalog) follow(peer []byte) {
	for {
		if c.relay.ctx.Err() != nil {
			return
		}

		c.mu.Lock()
		view := c.views[string(peer)]
		c.mu.Unlock()

		if view == nil {
			return
		}
		if err := c.ask(peer); err != nil {
			c.relay.log.Debug("the catalog of a peer did not arrive",
				"peer", identity.Name(peer), "error", err)
		}

		select {
		case <-time.After(CatalogPoll):
		case <-c.relay.ctx.Done():
			return
		}
	}
}

// ask reads one page of the catalog of a partner.
func (c *catalog) ask(peer []byte) error {
	c.mu.Lock()
	view := c.views[string(peer)]
	if view == nil {
		c.mu.Unlock()
		return ErrNoPeer
	}

	request := map[string]any{
		"type": "catalog", "version": 1, "mode": "snapshot",
		"epoch": nil, "revision": 0, "token": nil, "after": nil,
	}
	if view.token != "" {
		request["epoch"] = view.epoch
		request["token"] = view.token
		request["after"] = nullable(view.after)
	}
	c.mu.Unlock()

	reply, err := c.relay.federation.request(peer, request, federationRequestTimeout)
	if err != nil {
		return err
	}
	return c.take(peer, reply)
}

// take reads one page and puts what holds into the catalog.
func (c *catalog) take(peer []byte, reply map[string]any) error {
	if kind, _ := reply["type"].(string); kind != "catalog_reply" {
		c.forgetImports(peer)
		return ErrPeerUnavailable
	}
	if mode, _ := reply["mode"].(string); mode != "snapshot" {
		// A partner that answers a delta to a snapshot request is refused.
		c.forgetImports(peer)
		return ErrPeerUnavailable
	}

	records, _ := reply["records"].([]any)
	routes, _ := reply["routes"].(map[string]any)
	next, _ := reply["next"].(string)
	truncated, _ := reply["truncated"].(bool)
	epoch, _ := reply["epoch"].(string)
	token, _ := reply["token"].(string)

	now := time.Now()
	page := map[string]*imported{}

	for _, item := range records {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}

		entry, err := announce.Verify(record, now)
		if err != nil {
			continue
		}

		path, ok := c.routeOf(peer, entry, routes)
		if !ok {
			continue
		}

		page[string(entry.PublicKey)] = &imported{
			entry: entry, peer: peer, path: path,
			expiresAt: time.Unix(entry.ExpiresAt, 0),
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	view := c.views[string(peer)]
	if view == nil {
		return ErrNoPeer
	}

	if view.page == nil {
		view.page = map[string]*imported{}
	}
	for key, held := range page {
		view.page[key] = held
	}

	if next != "" {
		// The catalog of this partner runs over more pages.
		view.epoch = epoch
		view.token = token
		view.after = next
		return nil
	}

	// The last page of a snapshot replaces the view of this partner at once.
	c.imports[string(peer)] = view.page
	view.page = nil
	view.token = ""
	view.after = ""
	view.epoch = epoch
	view.synced = true
	view.truncated = truncated
	view.refreshed = now

	c.enforceLimit()
	return nil
}

// routeOf reads the path to one publisher, and checks it.
func (c *catalog) routeOf(peer []byte, entry *announce.Entry, routes map[string]any) ([][]byte, bool) {
	// Only a record that its publisher shared may travel.
	if entry.Federation == announce.Local {
		return nil, false
	}

	home := homeOf(entry)
	if len(home) != identity.SeedBytes {
		return nil, false
	}

	given, _ := routes[hex.EncodeToString(entry.PublicKey)].([]any)

	var path [][]byte
	for _, item := range given {
		text, ok := item.(string)
		if !ok {
			return nil, false
		}
		hop, err := hex.DecodeString(text)
		if err != nil || len(hop) != identity.SeedBytes {
			return nil, false
		}
		path = append(path, hop)
	}

	if len(path) == 0 {
		return nil, false
	}

	// The path begins at the partner that sent it, and ends at the home of
	// the publisher.
	if string(path[0]) != string(peer) || string(path[len(path)-1]) != string(home) {
		return nil, false
	}
	if len(path) > MaxRouteHops {
		return nil, false
	}

	// A record of a direct partner never travels further than that partner.
	if entry.Federation == announce.Direct && len(path) != 1 {
		return nil, false
	}

	seen := map[string]bool{string(c.relay.identity.PublicKey): true}
	for _, hop := range path {
		if seen[string(hop)] {
			return nil, false
		}
		seen[string(hop)] = true
	}
	return path, true
}

// answer serves the catalog of this relay to a partner.
func (c *catalog) answer(peer []byte, request map[string]any) map[string]any {
	if version, ok := wholeOf(request["version"]); !ok || version != 1 {
		return map[string]any{"ok": false, "error": "invalid_request"}
	}

	after, _ := request["after"].(string)
	entries := c.exportable(peer)

	var page []*imported
	for _, held := range entries {
		if after != "" && hex.EncodeToString(held.entry.PublicKey) <= after {
			continue
		}
		page = append(page, held)
	}

	more := len(page) > MaxCatalogPage
	if more {
		page = page[:MaxCatalogPage]
	}
	page = fitCatalogPage(page)

	records := make([]any, 0, len(page))
	routes := map[string]any{}

	for _, held := range page {
		records = append(records, held.entry.Record)

		hops := make([]any, 0, len(held.path))
		for _, hop := range held.path {
			hops = append(hops, hex.EncodeToString(hop))
		}
		routes[hex.EncodeToString(held.entry.PublicKey)] = hops
	}

	var next any
	if more && len(page) > 0 {
		next = hex.EncodeToString(page[len(page)-1].entry.PublicKey)
	}

	reply := map[string]any{
		"ok": true, "type": "catalog_reply", "version": 1, "mode": "snapshot",
		"epoch": c.epoch, "revision": 0, "token": c.epoch,
		"records": records, "routes": routes, "next": next, "truncated": false,
	}

	// A partner that asked for a delta takes a snapshot that replaces its
	// view.
	if mode, _ := request["mode"].(string); mode == "delta" {
		reply["reset"] = true
	}
	return reply
}

// exportable names what one partner may learn from this relay: the citizens
// here that shared beyond it, and, with transit, the records of other
// partners that their publishers shared with the network.
func (c *catalog) exportable(peer []byte) []*imported {
	now := time.Now()
	var out []*imported

	for _, held := range c.relay.match(func(held *record) bool {
		return held.entry.Federatable(c.relay.identity.PublicKey)
	}) {
		out = append(out, &imported{
			entry:     held.entry,
			path:      [][]byte{c.relay.identity.PublicKey},
			expiresAt: held.expiresAt,
		})
	}

	if c.relay.options.Transit {
		c.mu.Lock()
		for _, sources := range c.imports {
			for _, held := range sources {
				if held.entry.Federation != announce.Network || held.expiresAt.Before(now) {
					continue
				}
				// A record never goes back to a relay that is already on its
				// path, and never to the partner that sent it.
				if onPath(held.path, peer) || string(held.peer) == string(peer) {
					continue
				}
				if len(held.path)+1 > MaxRouteHops {
					continue
				}

				path := append([][]byte{c.relay.identity.PublicKey}, held.path...)
				out = append(out, &imported{entry: held.entry, path: path, expiresAt: held.expiresAt})
			}
		}
		c.mu.Unlock()
	}

	sort.Slice(out, func(left, right int) bool {
		return hex.EncodeToString(out[left].entry.PublicKey) < hex.EncodeToString(out[right].entry.PublicKey)
	})
	return out
}

// find returns what the catalog holds for a publisher, and the way there.
func (c *catalog) find(publicKey []byte) *imported {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	var best *imported
	for peer, sources := range c.imports {
		view := c.views[peer]
		if view == nil || !view.synced || view.refreshed.Add(CatalogLife).Before(now) {
			continue
		}

		held, there := sources[string(publicKey)]
		if !there || held.expiresAt.Before(now) {
			continue
		}
		if best == nil || len(held.path) < len(best.path) {
			best = held
		}
	}

	if held, there := c.live[string(publicKey)]; there {
		switch {
		case held.expiresAt.Before(now):
			delete(c.live, string(publicKey))
		case best == nil || len(held.path) < len(best.path):
			best = held
		}
	}
	return best
}

// matching reads the catalog for the entries that a query names.
func (c *catalog) matching(pass func(*announce.Entry) bool) []*imported {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	seen := map[string]*imported{}
	for peer, sources := range c.imports {
		view := c.views[peer]
		if view == nil || !view.synced || view.refreshed.Add(CatalogLife).Before(now) {
			continue
		}

		for key, held := range sources {
			if held.expiresAt.Before(now) || !pass(held.entry) {
				continue
			}
			if best, there := seen[key]; there && len(best.path) <= len(held.path) {
				continue
			}
			seen[key] = held
		}
	}

	out := make([]*imported, 0, len(seen))
	for _, held := range seen {
		out = append(out, held)
	}
	sort.Slice(out, func(left, right int) bool {
		return hex.EncodeToString(out[left].entry.PublicKey) < hex.EncodeToString(out[right].entry.PublicKey)
	})
	return out
}

// partial says whether a search may have missed something: a partner that is
// not synchronized, or a view that its partner cut short.
func (c *catalog) partial() bool {
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()

	for _, view := range c.views {
		if !view.synced || view.truncated || view.refreshed.Add(CatalogLife).Before(now) {
			return true
		}
	}
	return false
}

func (c *catalog) forgetImports(peer []byte) {
	c.mu.Lock()
	delete(c.imports, string(peer))
	if view := c.views[string(peer)]; view != nil {
		view.synced = false
		view.page = nil
		view.token = ""
		view.after = ""
	}
	c.mu.Unlock()
}

// enforceLimit holds the catalog inside its bound. The caller holds the lock.
func (c *catalog) enforceLimit() {
	total := 0
	for _, sources := range c.imports {
		total += len(sources)
	}
	if total <= MaxImported {
		return
	}

	// A view over the bound goes whole, so a half view never answers a
	// search as if it were complete.
	for peer, sources := range c.imports {
		if total <= MaxImported {
			return
		}
		total -= len(sources)
		delete(c.imports, peer)

		if view := c.views[peer]; view != nil {
			view.synced = false
			view.truncated = true
		}
	}
}

// fitCatalogPage drops records from the end until the page fits.
func fitCatalogPage(page []*imported) []*imported {
	for len(page) > 0 {
		records := make([]any, 0, len(page))
		for _, held := range page {
			records = append(records, held.entry.Record)
		}

		encoded, err := json.Marshal(records)
		if err == nil && len(encoded) <= MaxCatalogBytes {
			return page
		}
		page = page[:len(page)-1]
	}
	return page
}

func onPath(path [][]byte, key []byte) bool {
	for _, hop := range path {
		if string(hop) == string(key) {
			return true
		}
	}
	return false
}

func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func wholeOf(value any) (int, bool) {
	switch value := value.(type) {
	case json.Number:
		number, err := value.Int64()
		return int(number), err == nil
	case float64:
		return int(value), value == float64(int(value))
	case int:
		return value, true
	default:
		return 0, false
	}
}
