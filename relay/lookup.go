package relay

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/identity"
)

// A relay whose catalog cannot answer a lookup asks its partners at once:
// the bounded live lookup of docs/federation/SPEC.md.
//
// The request carries a network object: a random id, the relays that the
// lookup visited, and a budget of relays that may still answer. A relay that
// passes traffic on spends one of the budget on itself, and splits the rest
// among the partners that the path has not visited. A relay without transit
// answers for its own citizens only.
//
// The reply carries signed records. For each record it names the path from
// the relay that answers to the home of the publisher. A branch that does not
// answer, a reply that does not hold, an id that came back, and a budget that
// ran out each make the result partial.
const (
	// LookupBudget is the budget of a lookup at its origin, the origin
	// included.
	LookupBudget = 64
	// MaxLookupBranches is how many partners one relay asks for one lookup.
	MaxLookupBranches = 16
	// MaxLookupJobs is how many lookups one relay runs at one time.
	MaxLookupJobs = 32
	// LookupMemory is how long a relay refuses a lookup id that it saw.
	LookupMemory = 12 * time.Second

	// lookupTimeout is how long the origin waits for its partners. Each hop
	// further waits lookupHopStep less, so an answer can travel back in time.
	lookupTimeout = 8 * time.Second
	lookupHopStep = 800 * time.Millisecond
	lookupFloor   = time.Second
)

var lookupIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// lookups holds the ids that this relay answered lately, and the slots of the
// lookups that run.
type lookups struct {
	mu   sync.Mutex
	seen map[string]time.Time
	jobs chan struct{}
}

func newLookups() *lookups {
	return &lookups{seen: map[string]time.Time{}, jobs: make(chan struct{}, MaxLookupJobs)}
}

// remember says whether an id is new, and keeps it for LookupMemory.
func (l *lookups) remember(id string) bool {
	now := time.Now()

	l.mu.Lock()
	defer l.mu.Unlock()

	for held, at := range l.seen {
		if now.Sub(at) > LookupMemory {
			delete(l.seen, held)
		}
	}
	if _, there := l.seen[id]; there {
		return false
	}
	l.seen[id] = now
	return true
}

// begin takes the slot of one lookup, or says that every slot is taken.
func (l *lookups) begin() bool {
	select {
	case l.jobs <- struct{}{}:
		return true
	default:
		return false
	}
}

func (l *lookups) end() { <-l.jobs }

// hit is one record that a lookup found. A citizen of this relay has no
// peer and no path. A record of a partner has the partner that answered, and
// the path from that partner to the home of the publisher.
type hit struct {
	entry *announce.Entry
	peer  []byte
	path  [][]byte
}

// outcome is what a lookup found.
type outcome struct {
	hits    map[string]*hit
	partial bool
}

func (o *outcome) add(found *hit) {
	key := string(found.entry.PublicKey)
	if held, there := o.hits[key]; there && len(held.path) <= len(found.path) {
		return
	}
	o.hits[key] = found
}

// sorted returns the hits in the order of their public keys.
func (o *outcome) sorted() []*hit {
	out := make([]*hit, 0, len(o.hits))
	for _, found := range o.hits {
		out = append(out, found)
	}
	sort.Slice(out, func(left, right int) bool {
		return string(out[left].entry.PublicKey) < string(out[right].entry.PublicKey)
	})
	return out
}

// lookupTest returns the test of one kind of lookup.
func lookupTest(kind, query string) (func(*announce.Entry) bool, bool) {
	switch kind {
	case "resolve":
		return func(entry *announce.Entry) bool { return entry.Matches(query) }, true
	case "search":
		return func(entry *announce.Entry) bool {
			return len(entry.Capabilities) > 0 && entry.SearchMatch(query)
		}, true
	}
	return nil, false
}

// liveLookup runs one lookup from this relay, its origin.
func (r *Relay) liveLookup(kind, query string) outcome {
	raw := make([]byte, 16)
	rand.Read(raw)
	id := hex.EncodeToString(raw)

	r.lookups.remember(id)
	return r.askPartners(kind, query, id, nil, LookupBudget)
}

// askPartners sends a lookup to the partners that the path has not visited,
// and gathers what they find. visited holds the relays before this one, from
// the origin to the partner that asked. The budget counts this relay.
func (r *Relay) askPartners(kind, query, id string, visited [][]byte, budget int) outcome {
	result := outcome{hits: map[string]*hit{}}

	// A partner stands one connection further than this relay. Past the
	// bound, the lookup ends here.
	if len(visited)+1 > MaxRouteHops {
		return result
	}

	path := append(append([][]byte{}, visited...), r.identity.PublicKey)

	var ready [][]byte
	for _, peer := range r.federation.approved() {
		if onPath(path, peer) {
			continue
		}
		if _, err := r.federation.readyLink(peer); err != nil {
			result.partial = true
			continue
		}
		ready = append(ready, peer)
	}

	if len(ready) > MaxLookupBranches {
		ready = ready[:MaxLookupBranches]
		result.partial = true
	}

	remaining := budget - 1
	if len(ready) > remaining {
		ready = ready[:max(remaining, 0)]
		result.partial = true
	}
	if len(ready) == 0 {
		return result
	}

	hops := make([]any, 0, len(path))
	for _, hop := range path {
		hops = append(hops, hex.EncodeToString(hop))
	}
	timeout := max(lookupTimeout-time.Duration(len(visited))*lookupHopStep, lookupFloor)

	var mu sync.Mutex
	var group sync.WaitGroup

	for index, peer := range ready {
		share := remaining / len(ready)
		if index < remaining%len(ready) {
			share++
		}

		group.Add(1)
		go func() {
			defer group.Done()

			reply, err := r.federation.request(peer, map[string]any{
				"type":  kind,
				"query": query,
				"network": map[string]any{
					"id": id, "path": hops, "budget": share,
				},
			}, timeout)

			hits, partial := r.readLookupReply(peer, visited, reply, err)

			mu.Lock()
			defer mu.Unlock()
			result.partial = result.partial || partial
			for _, found := range hits {
				result.add(found)
			}
		}()
	}
	group.Wait()
	return result
}

// readLookupReply checks the answer of one partner. A record that does not
// hold goes, and makes the result partial.
func (r *Relay) readLookupReply(peer []byte, visited [][]byte, reply map[string]any, err error) ([]*hit, bool) {
	if err != nil || reply["ok"] != true {
		return nil, true
	}

	partial := reply["partial"] == true
	routes, _ := reply["routes"].(map[string]any)
	records, _ := reply["entries"].([]any)

	var hits []*hit
	for _, item := range records {
		record, ok := item.(map[string]any)
		if !ok {
			partial = true
			continue
		}

		entry, err := announce.Verify(record, time.Now())
		if err != nil {
			partial = true
			continue
		}

		path, ok := r.lookupRoute(peer, visited, entry, routes)
		if !ok {
			partial = true
			continue
		}
		hits = append(hits, &hit{entry: entry, peer: peer, path: path})
	}
	return hits, partial
}

// lookupRoute reads the path of one record: from the partner that answered
// to the home of the publisher. It never visits a relay twice, never crosses
// the path of the lookup, and keeps the whole way inside MaxRouteHops.
func (r *Relay) lookupRoute(peer []byte, visited [][]byte, entry *announce.Entry, routes map[string]any) ([][]byte, bool) {
	home := homeOf(entry)
	if len(home) != identity.SeedBytes {
		return nil, false
	}

	given, _ := routes[hex.EncodeToString(entry.PublicKey)].([]any)
	if len(given) == 0 || len(visited)+len(given) > MaxRouteHops {
		return nil, false
	}

	seen := map[string]bool{string(r.identity.PublicKey): true}
	for _, hop := range visited {
		seen[string(hop)] = true
	}

	path := make([][]byte, 0, len(given))
	for _, item := range given {
		text, _ := item.(string)
		hop, err := hex.DecodeString(text)
		if err != nil || len(hop) != identity.SeedBytes || seen[string(hop)] {
			return nil, false
		}
		seen[string(hop)] = true
		path = append(path, hop)
	}

	if string(path[0]) != string(peer) || string(path[len(path)-1]) != string(home) {
		return nil, false
	}

	// A record for direct partners crosses one connection: from its home to
	// the origin of the lookup.
	if entry.Federation == announce.Direct && (len(path) != 1 || len(visited) != 0) {
		return nil, false
	}
	return path, true
}

// answerLookup answers the lookup of a partner. It blocks while it asks the
// partners of this relay, so the federation runs it apart from the link.
func (r *Relay) answerLookup(peer []byte, request map[string]any) map[string]any {
	kind, _ := request["type"].(string)
	query, ok := request["query"].(string)
	if !ok || len(query) > MaxQueryBytes {
		return map[string]any{"ok": false, "error": "invalid_query"}
	}
	test, ok := lookupTest(kind, query)
	if !ok {
		return map[string]any{"ok": false, "error": "invalid_request"}
	}

	id, visited, budget, ok := r.readNetwork(peer, request["network"])
	if !ok {
		return map[string]any{"ok": false, "error": "invalid_request"}
	}

	empty := map[string]any{"ok": true, "entries": []any{}, "routes": map[string]any{}, "partial": true}
	if !r.lookups.remember(id) || !r.lookups.begin() {
		return empty
	}
	defer r.lookups.end()

	result := outcome{hits: map[string]*hit{}}

	// A citizen of this relay answers when it shares past this relay. A
	// record for direct partners reaches only the partner that asked first.
	for _, held := range r.match(func(held *record) bool {
		if !held.entry.Federatable(r.identity.PublicKey) || !test(held.entry) {
			return false
		}
		return held.entry.Federation == announce.Network || len(visited) == 1
	}) {
		result.add(&hit{entry: held.entry})
	}

	// Only a relay that passes traffic on asks further.
	if r.options.Transit {
		further := r.askPartners(kind, query, id, visited, budget)
		result.partial = result.partial || further.partial
		for _, found := range further.hits {
			result.add(found)
		}
	}

	return r.lookupReply(result)
}

// readNetwork checks the network object of a lookup: a fresh id, a path that
// ends at the partner that sent it and never names this relay, and a budget.
func (r *Relay) readNetwork(peer []byte, value any) (string, [][]byte, int, bool) {
	fields, ok := value.(map[string]any)
	if !ok {
		return "", nil, 0, false
	}

	id, _ := fields["id"].(string)
	budget, okBudget := wholeOf(fields["budget"])
	given, _ := fields["path"].([]any)
	if !lookupIDPattern.MatchString(id) || !okBudget || budget < 1 || budget > LookupBudget {
		return "", nil, 0, false
	}
	if len(given) == 0 || len(given) > MaxRouteHops {
		return "", nil, 0, false
	}

	seen := map[string]bool{string(r.identity.PublicKey): true}
	visited := make([][]byte, 0, len(given))
	for _, item := range given {
		text, _ := item.(string)
		hop, err := hex.DecodeString(text)
		if err != nil || len(hop) != identity.SeedBytes || seen[string(hop)] {
			return "", nil, 0, false
		}
		seen[string(hop)] = true
		visited = append(visited, hop)
	}

	if string(visited[len(visited)-1]) != string(peer) {
		return "", nil, 0, false
	}
	return id, visited, budget, true
}

// lookupReply writes the answer of a lookup: at most MaxDirectoryLimit
// records, each with the path from this relay to its home, inside the bound
// of one control message.
func (r *Relay) lookupReply(result outcome) map[string]any {
	hits := result.sorted()
	partial := result.partial
	if len(hits) > MaxDirectoryLimit {
		hits = hits[:MaxDirectoryLimit]
		partial = true
	}

	for {
		entries := make([]any, 0, len(hits))
		routes := map[string]any{}

		for _, found := range hits {
			entries = append(entries, found.entry.Record)

			route := []any{hex.EncodeToString(r.identity.PublicKey)}
			for _, hop := range found.path {
				route = append(route, hex.EncodeToString(hop))
			}
			routes[hex.EncodeToString(found.entry.PublicKey)] = route
		}

		answer := map[string]any{"ok": true, "entries": entries, "routes": routes, "partial": partial}
		if encoded, err := json.Marshal(answer); err == nil && len(encoded) <= MaxCatalogBytes {
			return answer
		}
		if len(hits) == 0 {
			return answer
		}
		hits = hits[:len(hits)-1]
		partial = true
	}
}
