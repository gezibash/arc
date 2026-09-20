package relay

import (
	"encoding/hex"
	"encoding/json"
	"regexp"
	"sort"
	"time"

	"github.com/gezibash/arc/go/announce"
	"github.com/gezibash/arc/go/internal/canonical"
	"github.com/gezibash/arc/go/internal/wire"
)

// The limits of the directory.
const (
	// DirectoryTTL is how long a relay holds a record, whatever the record says.
	DirectoryTTL = 180 * time.Second
	// MaxDirectoryRecords is how many records a relay holds.
	MaxDirectoryRecords = 10_000
	// MaxDirectoryLimit is the largest page of a search.
	MaxDirectoryLimit = 50
	// DefaultDirectoryLimit is the page of a search that asks for no limit.
	DefaultDirectoryLimit = 10
	// MaxDirectoryReplyBytes caps one reply of the directory.
	MaxDirectoryReplyBytes = 240 * 1024
	// MaxQueryBytes caps one query.
	MaxQueryBytes = 256
)

var (
	requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)
	cursorPattern    = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// record is one announcement that the relay holds.
type record struct {
	entry     *announce.Entry
	conn      *conn
	expiresAt time.Time
	cursor    string
}

// directoryControl answers one control message. A message that is not valid
// gets no answer, because the relay says nothing about what it refuses to
// parse.
func (r *Relay) directoryControl(from *conn, payload []byte) {
	value, err := canonical.Decode(payload)
	if err != nil {
		return
	}

	request, ok := value.(map[string]any)
	if !ok {
		return
	}

	requestID, ok := request["request_id"].(string)
	if !ok || !requestIDPattern.MatchString(requestID) {
		return
	}

	switch request["type"] {
	case "announce":
		r.announceRecord(from, requestID, request["record"])
	case "resolve":
		r.resolve(from, requestID, request)
	case "search":
		r.search(from, requestID, request)
	case "observe":
		r.observe(from, requestID, request)
	case "status":
		r.status(from, requestID, request)
	default:
		r.reply(from, requestID, map[string]any{"ok": false, "error": "invalid_request"})
	}
}

func (r *Relay) announceRecord(from *conn, requestID string, raw any) {
	fields, ok := raw.(map[string]any)
	if !ok {
		r.refuse(from, requestID, "invalid_announcement")
		return
	}

	entry, err := announce.Verify(fields, time.Now())
	if err != nil {
		r.refuse(from, requestID, "invalid_announcement")
		return
	}

	// A citizen announces only under its own key, and a federated record
	// names this relay as its home.
	if string(entry.PublicKey) != string(from.publicKey) {
		r.refuse(from, requestID, "invalid_announcement")
		return
	}
	if entry.Federation != announce.Local && string(entry.RelayPublicKey) != string(r.identity.PublicKey) {
		r.refuse(from, requestID, "invalid_announcement")
		return
	}

	expires := time.Now().Add(DirectoryTTL)
	if held := time.Unix(entry.ExpiresAt, 0); held.Before(expires) {
		expires = held
	}

	r.mu.Lock()
	key := string(entry.PublicKey)
	delete(r.directory, key)

	// A citizen that is connected may replace its own listing. A full
	// directory never drops the live offer of another citizen.
	if len(r.directory) >= MaxDirectoryRecords {
		r.mu.Unlock()
		r.refuse(from, requestID, "directory_full")
		return
	}

	r.directory[key] = &record{
		entry:     entry,
		conn:      from,
		expiresAt: expires,
		cursor:    hex.EncodeToString(entry.PublicKey),
	}
	r.mu.Unlock()

	r.reply(from, requestID, map[string]any{"ok": true})
}

func (r *Relay) resolve(from *conn, requestID string, request map[string]any) {
	query, ok := query(request)
	if !ok {
		r.refuse(from, requestID, "invalid_query")
		return
	}

	matching := r.match(func(held *record) bool { return held.entry.Matches(query) })
	if len(matching) > 2 {
		matching = matching[:2]
	}

	r.reply(from, requestID, map[string]any{"ok": true, "entries": recordsOf(matching)})
}

func (r *Relay) search(from *conn, requestID string, request map[string]any) {
	query, ok := query(request)
	if !ok {
		r.refuse(from, requestID, "invalid_query")
		return
	}

	after, ok := cursor(request["after"])
	if !ok {
		r.refuse(from, requestID, "invalid_query")
		return
	}

	matching := r.match(func(held *record) bool {
		return len(held.entry.Capabilities) > 0 && held.entry.SearchMatch(query)
	})
	total := len(matching)

	if after != "" {
		for len(matching) > 0 && matching[0].cursor <= after {
			matching = matching[1:]
		}
	}

	limit := DefaultDirectoryLimit
	if given, ok := whole(request["limit"]); ok && given > 0 {
		limit = min(given, MaxDirectoryLimit)
	}

	page := matching
	if len(page) > limit {
		page = page[:limit]
	}
	page = fit(page, requestID, total)

	next := any(nil)
	if len(page) > 0 && len(matching) > len(page) {
		next = page[len(page)-1].cursor
	}

	r.reply(from, requestID, map[string]any{
		"ok":      true,
		"entries": recordsOf(page),
		"next":    next,
		"total":   total,
	})
}

func (r *Relay) observe(from *conn, requestID string, request map[string]any) {
	if len(request) != 2 {
		r.refuse(from, requestID, "invalid_request")
		return
	}

	host, port := from.endpoint()
	if host == "" {
		r.refuse(from, requestID, "observation_unavailable")
		return
	}

	r.reply(from, requestID, map[string]any{
		"ok":       true,
		"observed": map[string]any{"host": host, "port": port},
	})
}

func (r *Relay) status(from *conn, requestID string, request map[string]any) {
	if len(request) != 2 {
		r.refuse(from, requestID, "invalid_request")
		return
	}

	r.reply(from, requestID, map[string]any{
		"ok": true,
		"status": map[string]any{
			"role":               "relay",
			"state":              "running",
			"version":            r.options.Version,
			"public_key":         hex.EncodeToString(r.identity.PublicKey),
			"uptime_seconds":     int(time.Since(r.started).Seconds()),
			"federation_transit": false,
		},
	})
}

// match returns the live records that pass the test, in cursor order.
func (r *Relay) match(pass func(*record) bool) []*record {
	now := time.Now()

	r.mu.Lock()
	defer r.mu.Unlock()

	matching := make([]*record, 0, 8)
	for key, held := range r.directory {
		if held.expiresAt.Before(now) || r.routes[key] != held.conn {
			delete(r.directory, key)
			continue
		}
		if pass(held) {
			matching = append(matching, held)
		}
	}

	sort.Slice(matching, func(left, right int) bool {
		return matching[left].cursor < matching[right].cursor
	})
	return matching
}

// fit drops entries from the end until the reply stands inside its cap.
func fit(page []*record, requestID string, total int) []*record {
	for len(page) > 0 {
		reply := map[string]any{
			"type":       "reply",
			"request_id": requestID,
			"ok":         true,
			"entries":    recordsOf(page),
			"next":       nil,
			"total":      total,
		}

		encoded, err := json.Marshal(reply)
		if err == nil && len(encoded) <= MaxDirectoryReplyBytes {
			return page
		}
		page = page[:len(page)-1]
	}
	return page
}

func recordsOf(records []*record) []any {
	out := make([]any, 0, len(records))
	for _, held := range records {
		out = append(out, held.entry.Record)
	}
	return out
}

func (r *Relay) reply(to *conn, requestID string, fields map[string]any) {
	reply := map[string]any{"type": "reply", "request_id": requestID}
	for key, value := range fields {
		reply[key] = value
	}

	payload, err := json.Marshal(reply)
	if err != nil {
		r.log.Error("the reply does not encode", "error", err)
		return
	}
	to.send(wire.Directory(payload))
}

func (r *Relay) refuse(to *conn, requestID, reason string) {
	r.reply(to, requestID, map[string]any{"ok": false, "error": reason})
}

func query(request map[string]any) (string, bool) {
	query, ok := request["query"].(string)
	if !ok || len(query) > MaxQueryBytes {
		return "", false
	}
	return query, true
}

func cursor(value any) (string, bool) {
	switch value := value.(type) {
	case nil:
		return "", true
	case string:
		if cursorPattern.MatchString(value) {
			return value, true
		}
	}
	return "", false
}

func whole(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	out, err := number.Int64()
	if err != nil {
		return 0, false
	}
	return int(out), true
}
