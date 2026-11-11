// Package rings makes a khatru relay ring a citizen: one HTTPS call to a URL
// that the citizen registered, when mail for the citizen waits unread, as
// docs/rings/SPEC.md defines.
//
// A citizen registers with PUT /ring, signed with NIP-98 as GET /turn is.
// The relay keeps the ring key encrypted in its store, with a secret that
// only the relay holds. It rings once for each unread period: from the
// first relay-form wrap of kind 1059 after the last read of the citizen,
// until the next read. A read is a subscription for the wraps of the
// citizen, on a connection that authenticated as the citizen, or as a
// device with a token of the citizen. The call carries no mail, no author
// and no count.
package rings

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"go.etcd.io/bbolt"
)

// Tag is the tag of the NIP-11 document of a relay that offers rings.
const Tag = "arc-rings"

// Path is the HTTP path of a registration, and TestPath the path that
// asks for one ring now.
const (
	Path     = "/ring"
	TestPath = "/ring/test"
)

// TokenHeader is the HTTP header with which a device key presents a token
// of its citizen: the token event as base64 JSON.
const TokenHeader = "X-Arc-Token" //nolint:gosec // The name of a header, not a credential.

// The limits of a registration (docs/rings/SPEC.md, section 3.2).
const (
	MinKey             = 16
	MaxKey             = 512
	MinInterval        = 60
	MaxInterval        = 86400
	DefaultInterval    = 300
	MaxBurst           = 10
	DefaultBurst       = 3
	DefaultDailyCap    = 48
	DefaultPerMinute   = 600
	DefaultTTL         = 30 * 24 * time.Hour
	RingOnAny          = "any"
	RingOnStamped      = "stamped"
	maxURL             = 2048
	maxBody            = 8 << 10
	answerBytes        = 4 << 10
	callTimeout        = 10 * time.Second
	defaultPort        = 443
	bucketName         = "rings"
	keyInfo            = "arc-rings-key-v1\x00"
	sweepEvery         = time.Hour
	errNoRegistration  = "no registration"
	errStampedNotBuilt = "ring_on stamped needs the stamp rule of the relay, which this relay does not have"
)

// IDHeader is the HTTP header that names a ring, as in Standard Webhooks.
// Each try of one ring carries the same ID.
const IDHeader = "Webhook-Id"

// Settings are the parts of a registration that the relay shows.
type Settings struct {
	URL string `json:"url"`
	// MinInterval is the seconds in which one ring comes back to the
	// bucket of the citizen.
	MinInterval int `json:"min_interval"`
	// Burst is the most rings that the bucket holds: the rings that can go
	// one after the other after a quiet time.
	Burst    int    `json:"burst"`
	DailyCap int    `json:"daily_cap"`
	RingOn   string `json:"ring_on"`
}

// Request is the body of PUT /ring.
type Request struct {
	Settings
	Key string `json:"key"`
}

// Status is the answer to GET and PUT /ring. It never holds the key.
type Status struct {
	Settings
	Citizen string `json:"citizen"`
	// Expires is the Unix time at which the registration ends, unless the
	// citizen reads or sets it again.
	Expires int64 `json:"expires"`
	// LastRing is the Unix time of the last ring, or zero.
	LastRing int64 `json:"last_ring"`
	// LastStatus is the HTTP status of the last ring, zero for no answer.
	LastStatus int `json:"last_status"`
	Left       int `json:"left_today"`
}

// TestAnswer is the answer to POST /ring/test.
type TestAnswer struct {
	// Status is the HTTP status that the ring URL gave, zero for none.
	Status int    `json:"status"`
	Error  string `json:"error,omitempty"`
}

// call is the body of a ring. Its fields keep this order.
type call struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Citizen string `json:"citizen"`
	Relay   string `json:"relay"`
	Since   int64  `json:"since"`
}

// Config holds the settings of the rings of a relay.
type Config struct {
	// Secret encrypts the ring keys in the store. It is required.
	Secret string
	// DailyCap is the largest daily_cap that a citizen can set. Zero is
	// DefaultDailyCap.
	DailyCap int
	// PerMinute caps the rings of the whole relay in each minute. Zero is
	// DefaultPerMinute.
	PerMinute int
	// TTL is how long a registration lives after the last read of its
	// citizen, or its last PUT. Zero is DefaultTTL.
	TTL time.Duration
	// Ports are the ports that a ring URL can name. Empty is 443 only.
	Ports []int
	// Own are the public addresses of the relay, which a ring URL must not
	// name. The addresses of the interfaces of the machine are added.
	Own []netip.Addr
	// Readers returns the keys that a connection reads for: each key that
	// authenticated, and each citizen of a device token.
	Readers func(ws *khatru.WebSocket) []nostr.PubKey
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
	// Lookup resolves the host of a ring URL. Nil is the system resolver.
	Lookup func(ctx context.Context, host string) ([]netip.Addr, error)
	// Dial connects to an address that passed the checks. Nil is a
	// net.Dialer that checks the address again.
	Dial func(ctx context.Context, network, address string) (net.Conn, error)
	// TLS is the TLS configuration of a ring. Nil is the default.
	TLS *tls.Config
	// Logger gets one line for each try of a ring, and one for each wrap
	// that rings no one now, with its reason. A line names a citizen only
	// by the first 8 hex digits of its key, and holds no ring key, URL,
	// host or wrap.
	Logger *slog.Logger
}

// Rings holds the registrations of a relay and rings them.
//
// A ring starts on the save of a wrap, in the hook of khatru. A ring that
// must wait, for a token of the bucket, the cap of the relay, the next day
// or a retry, has one timer. No loop polls.
type Rings struct {
	cfg    Config
	db     *bbolt.DB
	aead   cipher.AEAD
	client *http.Client
	own    map[netip.Addr]bool

	mu       sync.Mutex
	by       map[nostr.PubKey]*state
	watching map[nostr.PubKey]int
	subs     map[*khatru.WebSocket]map[int][]nostr.PubKey
	// told holds the notices that each connection was told for each
	// citizen, so that a sync of many queries gets each notice once.
	told   map[*khatru.WebSocket]map[noticeKey]bool
	busy   map[nostr.PubKey]bool
	timers map[nostr.PubKey]*time.Timer
	// relay is the bucket of the whole relay: PerMinute tokens, which come
	// back over one minute.
	relayTokens   float64
	relayRefilled time.Time
	// sends counts the tries in flight, so that a test can wait for them.
	sends sync.WaitGroup
}

// Apply makes the relay offer rings, with the registrations in db. Call it
// after the relay has its event store. Each ring that the store holds
// starts again.
func Apply(rl *khatru.Relay, db *bbolt.DB, cfg Config) (*Rings, error) {
	if cfg.Secret == "" {
		return nil, errors.New("rings need a secret")
	}
	if cfg.DailyCap <= 0 {
		cfg.DailyCap = DefaultDailyCap
	}
	if cfg.PerMinute <= 0 {
		cfg.PerMinute = DefaultPerMinute
	}
	if cfg.TTL <= 0 {
		cfg.TTL = DefaultTTL
	}
	if len(cfg.Ports) == 0 {
		cfg.Ports = []int{defaultPort}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Lookup == nil {
		cfg.Lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	if cfg.Readers == nil {
		cfg.Readers = func(ws *khatru.WebSocket) []nostr.PubKey { return ws.Authed() }
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	secret := sha256.Sum256([]byte(keyInfo + cfg.Secret))
	block, err := aes.NewCipher(secret[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	r := &Rings{
		cfg: cfg, db: db, aead: aead, own: ownAddrs(cfg.Own),
		by: map[nostr.PubKey]*state{}, watching: map[nostr.PubKey]int{},
		subs: map[*khatru.WebSocket]map[int][]nostr.PubKey{}, busy: map[nostr.PubKey]bool{},
		told:        map[*khatru.WebSocket]map[noticeKey]bool{},
		timers:      map[nostr.PubKey]*time.Timer{},
		relayTokens: float64(cfg.PerMinute), relayRefilled: cfg.Now(),
	}
	r.client = r.newClient()
	if err := r.load(); err != nil {
		return nil, err
	}
	r.hook(rl)
	r.mu.Lock()
	r.sweep()
	for pk := range r.by {
		r.schedule(pk)
	}
	r.mu.Unlock()
	return r, nil
}

// load reads the registrations from the store. A registration of v0.33.0
// has no burst and no expiry: it gets the default burst, and lives for the
// TTL from its creation.
func (r *Rings) load() error {
	return r.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketName))
		if err != nil {
			return err
		}
		return b.ForEach(func(k, v []byte) error {
			pk, err := nostr.PubKeyFromHex(string(k))
			if err != nil {
				return nil
			}
			var s state
			if json.Unmarshal(v, &s) != nil {
				return nil
			}
			if s.Burst == 0 {
				s.Burst = DefaultBurst
			}
			if s.Expires == 0 {
				s.Expires = time.Unix(s.Created, 0).Add(r.cfg.TTL).Unix()
			}
			r.by[pk] = &s
			return nil
		})
	})
}

// save writes the registration of a citizen, or removes it when s is nil.
// The caller holds r.mu.
func (r *Rings) save(pk nostr.PubKey, s *state) {
	err := r.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketName))
		if s == nil {
			return b.Delete([]byte(pk.Hex()))
		}
		data, err := json.Marshal(s)
		if err != nil {
			return err
		}
		return b.Put([]byte(pk.Hex()), data)
	})
	if err != nil {
		r.cfg.Logger.Warn("rings: the store refused a write", "error", err)
	}
}

// remove ends the registration of a citizen. The caller holds r.mu.
func (r *Rings) remove(pk nostr.PubKey) {
	delete(r.by, pk)
	r.stop(pk)
	r.save(pk, nil)
}

// short names a citizen in the log: the first 8 hex digits of its key.
func short(pk nostr.PubKey) string { return pk.Hex()[:8] }

// Run sweeps the registrations once an hour, until ctx ends. Rings do not
// wait for it.
func (r *Rings) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.mu.Lock()
			r.sweep()
			r.mu.Unlock()
		case <-ctx.Done():
			r.mu.Lock()
			for pk := range r.timers {
				r.stop(pk)
			}
			r.mu.Unlock()
			return
		}
	}
}

// sweep renews each registration whose citizen has a read subscription
// open, and removes each registration that expired, or that got no 2xx
// answer for 7 days. The caller holds r.mu.
func (r *Rings) sweep() {
	now := r.cfg.Now()
	for pk, s := range r.by {
		if r.watching[pk] > 0 {
			s.renew(now, r.cfg.TTL)
			r.save(pk, s)
		}
		switch {
		case s.expired(now):
			r.remove(pk)
			r.cfg.Logger.Info("rings: removed a registration that expired", "citizen", short(pk))
		case s.abandoned(now):
			r.remove(pk)
			r.cfg.Logger.Info("rings: removed a registration after 7 days with no 2xx answer", "citizen", short(pk))
		}
	}
}

// seal encrypts a ring key with the secret of the relay.
func (r *Rings) seal(key string) ([]byte, error) {
	nonce := make([]byte, r.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return r.aead.Seal(nonce, nonce, []byte(key), nil), nil
}

func (r *Rings) open(sealed []byte) ([]byte, error) {
	n := r.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("the stored ring key is damaged")
	}
	key, err := r.aead.Open(nil, sealed[:n], sealed[n:], nil)
	if err != nil {
		return nil, errors.New("the stored ring key does not open; did the secret change?")
	}
	return key, nil
}
