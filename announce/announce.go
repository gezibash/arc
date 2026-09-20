// Package announce holds the signed, short-lived announcement that a citizen
// publishes to a relay.
//
// An announcement is a small public document. It says which identity offers
// which capabilities, and where the signed detail of each capability is asked
// for. It never holds a secret key, and never an endpoint that a caller chose.
//
// The signature covers a domain string and the canonical JSON of the record
// without its signature:
//
//	signature = Sign(secret, "arc-relay-announcement-v1\n" + canonical(unsigned))
//
// Version 1 stays on its relay. Version 2 ("direct") and version 3 ("network")
// carry the public key of the relay that may pass them on.
package announce

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/canonical"
)

// The limits of an announcement. A relay refuses a record that breaks one.
const (
	DefaultTTL      = 180 * time.Second
	MaxTTL          = 180 * time.Second
	MaxFutureSkew   = 30 * time.Second
	MaxRecordBytes  = 8192
	MaxCapabilities = 8
)

// Federation says how far an announcement travels.
type Federation string

// The three reaches of an announcement.
const (
	// Local stays on the relay that receives it.
	Local Federation = "local"
	// Direct travels to the relays that federate with the home relay.
	Direct Federation = "direct"
	// Network travels across the federation.
	Network Federation = "network"
)

// ErrInvalid reports a record that does not verify.
var ErrInvalid = errors.New("announce: invalid announcement")

var domains = map[int]string{
	1: "arc-relay-announcement-v1\n",
	2: "arc-relay-announcement-v2\n",
	3: "arc-relay-announcement-v3\n",
}

var versions = map[Federation]int{Local: 1, Direct: 2, Network: 3}

// limits holds the length of each capability field in bytes.
var limits = map[string]int{
	"id":              128,
	"kind":            128,
	"scheme":          128,
	"title":           256,
	"summary":         1024,
	"invocation_mode": 256,
	"release_version": 256,
	"channel":         256,
	"detail_path":     256,
}

// Capability is one public summary of one capability. Only the id is needed.
type Capability struct {
	ID             string
	Kind           string
	Scheme         string
	Title          string
	Summary        string
	InvocationMode string
	ReleaseVersion string
	Channel        string
	DetailPath     string
}

func (c Capability) fields() map[string]string {
	return map[string]string{
		"id":              c.ID,
		"kind":            c.Kind,
		"scheme":          c.Scheme,
		"title":           c.Title,
		"summary":         c.Summary,
		"invocation_mode": c.InvocationMode,
		"release_version": c.ReleaseVersion,
		"channel":         c.Channel,
		"detail_path":     c.DetailPath,
	}
}

// Entry is one verified announcement.
type Entry struct {
	PublicKey      []byte
	Name           string
	ShortName      string
	Capabilities   []Capability
	IssuedAt       int64
	ExpiresAt      int64
	Federation     Federation
	RelayPublicKey []byte
	Record         map[string]any
}

// Options changes one field of a new announcement.
type Options struct {
	// Now is the time of issue. The zero time means the time of the call.
	Now time.Time
	// TTL is the lifetime. Zero means DefaultTTL. The limit is MaxTTL.
	TTL time.Duration
	// Federation is the reach. The empty value means Local.
	Federation Federation
	// RelayPublicKey is the home relay. A reach above Local needs it.
	RelayPublicKey []byte
}

// Create builds one signed announcement.
func Create(me *identity.Identity, capabilities []Capability, opts Options) (map[string]any, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}

	ttl := opts.TTL
	if ttl == 0 {
		ttl = DefaultTTL
	}
	if ttl <= 0 || ttl > MaxTTL {
		return nil, fmt.Errorf("%w: the lifetime must be 1 to %d seconds", ErrInvalid, int(MaxTTL.Seconds()))
	}

	if len(capabilities) > MaxCapabilities {
		return nil, fmt.Errorf("%w: at most %d capabilities", ErrInvalid, MaxCapabilities)
	}

	list := make([]any, 0, len(capabilities))
	for _, capability := range capabilities {
		fields, err := normalize(capability)
		if err != nil {
			return nil, err
		}
		list = append(list, fields)
	}

	reach := opts.Federation
	if reach == "" {
		reach = Local
	}
	version, ok := versions[reach]
	if !ok {
		return nil, fmt.Errorf("%w: unknown federation %q", ErrInvalid, reach)
	}

	issuedAt := now.Unix()
	unsigned := map[string]any{
		"version":      version,
		"public_key":   me.EncodePublicKey(),
		"capabilities": list,
		"issued_at":    issuedAt,
		"expires_at":   issuedAt + int64(ttl.Seconds()),
	}

	if reach != Local {
		if len(opts.RelayPublicKey) != identity.SeedBytes {
			return nil, fmt.Errorf("%w: a federated announcement needs the relay public key", ErrInvalid)
		}
		unsigned["federation"] = string(reach)
		unsigned["relay_public_key"] = hex.EncodeToString(opts.RelayPublicKey)
	}

	payload, err := signingPayload(unsigned, version)
	if err != nil {
		return nil, err
	}

	record := make(map[string]any, len(unsigned)+1)
	for key, value := range unsigned {
		record[key] = value
	}
	record["signature"] = hex.EncodeToString(me.Sign(payload))

	encoded, err := canonical.Encode(record)
	if err != nil {
		return nil, err
	}
	if len(encoded) > MaxRecordBytes {
		return nil, fmt.Errorf("%w: the record is over %d bytes", ErrInvalid, MaxRecordBytes)
	}
	return record, nil
}

// Verify checks the signature of a record and its short lifetime. A zero now
// means the time of the call.
func Verify(record map[string]any, now time.Time) (*Entry, error) {
	if now.IsZero() {
		now = time.Now()
	}

	version, reach, relayKey, err := readVersion(record)
	if err != nil {
		return nil, err
	}

	keys := []string{"version", "public_key", "capabilities", "issued_at", "expires_at", "signature"}
	if reach != Local {
		keys = append(keys, "federation", "relay_public_key")
	}
	if !sameKeys(record, keys) {
		return nil, ErrInvalid
	}

	publicKey, err := readHex(record["public_key"], identity.SeedBytes)
	if err != nil {
		return nil, err
	}
	signature, err := readHex(record["signature"], 64)
	if err != nil {
		return nil, err
	}

	issuedAt, err := readWholeNumber(record["issued_at"])
	if err != nil {
		return nil, err
	}
	expiresAt, err := readWholeNumber(record["expires_at"])
	if err != nil {
		return nil, err
	}

	capabilities, err := readCapabilities(record["capabilities"])
	if err != nil {
		return nil, err
	}

	encoded, err := canonical.Encode(record)
	if err != nil || len(encoded) > MaxRecordBytes {
		return nil, ErrInvalid
	}

	seconds := now.Unix()
	life := expiresAt - issuedAt
	if issuedAt > seconds+int64(MaxFutureSkew.Seconds()) || expiresAt <= seconds {
		return nil, ErrInvalid
	}
	if life <= 0 || life > int64(MaxTTL.Seconds()) {
		return nil, ErrInvalid
	}

	unsigned := make(map[string]any, len(record)-1)
	for key, value := range record {
		if key != "signature" {
			unsigned[key] = value
		}
	}

	payload, err := signingPayload(unsigned, version)
	if err != nil {
		return nil, err
	}
	if !identity.Verify(publicKey, payload, signature) {
		return nil, ErrInvalid
	}

	return &Entry{
		PublicKey:      publicKey,
		Name:           identity.Name(publicKey),
		ShortName:      identity.ShortName(publicKey),
		Capabilities:   capabilities,
		IssuedAt:       issuedAt,
		ExpiresAt:      expiresAt,
		Federation:     reach,
		RelayPublicKey: relayKey,
		Record:         record,
	}, nil
}

// Federatable says whether this announcement may leave its home relay.
func (e *Entry) Federatable(originRelayKey []byte) bool {
	if e.Federation == Local || len(originRelayKey) != identity.SeedBytes {
		return false
	}
	return len(e.RelayPublicKey) == identity.SeedBytes &&
		string(e.RelayPublicKey) == string(originRelayKey)
}

// Matches answers a resolve: the full petname, the short petname, or the start
// of the public key.
func (e *Entry) Matches(query string) bool {
	if query == "" {
		return false
	}
	query = strings.ToLower(query)

	return query == strings.ToLower(e.Name) ||
		query == strings.ToLower(e.ShortName) ||
		strings.HasPrefix(hex.EncodeToString(e.PublicKey), query)
}

// SearchMatch answers a search over the public fields of each capability. An
// empty query matches every entry that offers a capability.
func (e *Entry) SearchMatch(query string) bool {
	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return len(e.Capabilities) > 0
	}

	for _, capability := range e.Capabilities {
		words := []string{e.Name, e.ShortName}
		for _, value := range capability.fields() {
			if value != "" {
				words = append(words, value)
			}
		}

		haystack := strings.ToLower(strings.Join(words, " "))
		found := true
		for _, token := range tokens {
			if !strings.Contains(haystack, token) {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

// Provider returns the identity shape that manifests and discovery share.
func (e *Entry) Provider() map[string]any {
	return map[string]any{
		"public_key": hex.EncodeToString(e.PublicKey),
		"name":       e.Name,
		"short_name": e.ShortName,
	}
}

func signingPayload(unsigned map[string]any, version int) ([]byte, error) {
	domain, ok := domains[version]
	if !ok {
		return nil, ErrInvalid
	}

	encoded, err := canonical.Encode(unsigned)
	if err != nil {
		return nil, err
	}
	return append([]byte(domain), encoded...), nil
}

func normalize(capability Capability) (map[string]any, error) {
	fields := map[string]any{}
	for key, value := range capability.fields() {
		if value == "" {
			if key == "id" {
				return nil, fmt.Errorf("%w: a capability needs an id", ErrInvalid)
			}
			continue
		}
		if len(value) > limits[key] {
			return nil, fmt.Errorf("%w: the capability field %s is too long", ErrInvalid, key)
		}
		fields[key] = value
	}
	if _, ok := fields["id"]; !ok {
		return nil, fmt.Errorf("%w: a capability needs an id", ErrInvalid)
	}
	return fields, nil
}

func readVersion(record map[string]any) (int, Federation, []byte, error) {
	version, err := readWholeNumber(record["version"])
	if err != nil {
		return 0, "", nil, err
	}

	switch version {
	case 1:
		if _, ok := record["federation"]; ok {
			return 0, "", nil, ErrInvalid
		}
		return 1, Local, nil, nil
	case 2, 3:
		reach := Direct
		if version == 3 {
			reach = Network
		}
		if name, ok := record["federation"].(string); !ok || Federation(name) != reach {
			return 0, "", nil, ErrInvalid
		}
		relayKey, err := readHex(record["relay_public_key"], identity.SeedBytes)
		if err != nil {
			return 0, "", nil, err
		}
		return int(version), reach, relayKey, nil
	default:
		return 0, "", nil, ErrInvalid
	}
}

func readCapabilities(value any) ([]Capability, error) {
	list, ok := value.([]any)
	if !ok || len(list) > MaxCapabilities {
		return nil, ErrInvalid
	}

	out := make([]Capability, 0, len(list))
	for _, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			return nil, ErrInvalid
		}

		flat := map[string]string{}
		for key, raw := range fields {
			limit, known := limits[key]
			text, isText := raw.(string)
			if !known || !isText || len(text) > limit {
				return nil, ErrInvalid
			}
			if text == "" && key == "id" {
				return nil, ErrInvalid
			}
			if text != "" {
				flat[key] = text
			}
		}
		if flat["id"] == "" {
			return nil, ErrInvalid
		}

		out = append(out, Capability{
			ID:             flat["id"],
			Kind:           flat["kind"],
			Scheme:         flat["scheme"],
			Title:          flat["title"],
			Summary:        flat["summary"],
			InvocationMode: flat["invocation_mode"],
			ReleaseVersion: flat["release_version"],
			Channel:        flat["channel"],
			DetailPath:     flat["detail_path"],
		})
	}
	return out, nil
}

// readWholeNumber reads a whole number from a decoded record. The decoder
// keeps the digits, so a value with a fraction is an error.
func readWholeNumber(value any) (int64, error) {
	switch value := value.(type) {
	case json.Number:
		out, err := value.Int64()
		if err != nil {
			return 0, ErrInvalid
		}
		return out, nil
	case int:
		return int64(value), nil
	case int64:
		return value, nil
	default:
		return 0, ErrInvalid
	}
}

func readHex(value any, want int) ([]byte, error) {
	text, ok := value.(string)
	if !ok || len(text) != want*2 {
		return nil, ErrInvalid
	}
	out, err := hex.DecodeString(text)
	if err != nil || len(out) != want {
		return nil, ErrInvalid
	}
	return out, nil
}

func sameKeys(record map[string]any, keys []string) bool {
	if len(record) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := record[key]; !ok {
			return false
		}
	}
	return true
}
