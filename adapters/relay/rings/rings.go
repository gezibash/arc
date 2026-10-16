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
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"syscall"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/private"
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
	DefaultDailyCap    = 48
	DefaultPerMinute   = 600
	RingOnAny          = "any"
	RingOnStamped      = "stamped"
	maxURL             = 2048
	maxBody            = 8 << 10
	answerBytes        = 4 << 10
	callTimeout        = 10 * time.Second
	defaultPort        = 443
	bucketName         = "rings"
	keyInfo            = "arc-rings-key-v1\x00"
	tickEvery          = time.Second
	errNoRegistration  = "no registration"
	errStampedNotBuilt = "ring_on stamped needs the stamp rule of the relay, which this relay does not have"
)

// Settings are the parts of a registration that the relay shows.
type Settings struct {
	URL         string `json:"url"`
	MinInterval int    `json:"min_interval"`
	DailyCap    int    `json:"daily_cap"`
	RingOn      string `json:"ring_on"`
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
	// Logger gets one line for each ring, with no key, URL, host or
	// citizen.
	Logger *slog.Logger
}

// Rings holds the registrations of a relay and rings them.
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
	busy     map[nostr.PubKey]bool
	minute   int64
	inMinute int
}

// Apply makes the relay offer rings, with the registrations in db. Call it
// after the relay has its event store. Run must run for the relay to ring.
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
		cfg.Readers = func(ws *khatru.WebSocket) []nostr.PubKey { return ws.AuthedPublicKeys }
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
	}
	r.client = r.newClient()
	if err := r.load(); err != nil {
		return nil, err
	}
	r.hook(rl)
	return r, nil
}

// load reads the registrations from the store.
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
			if json.Unmarshal(v, &s) == nil {
				r.by[pk] = &s
			}
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
	removed := rl.OnListenerRemoved
	rl.OnListenerRemoved = func(ws *khatru.WebSocket, ssid int, id string, filter nostr.Filter) {
		r.unlisten(ws, ssid)
		if removed != nil {
			removed(ws, ssid, id, filter)
		}
	}
}

// wrap notes a relay-form wrap. A courier-form wrap has no p tag, and a
// live wrap is ephemeral and never saved, so neither rings.
func (r *Rings) wrap(event nostr.Event) {
	now := r.cfg.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for tag := range event.Tags.FindAll("p") {
		pk, err := nostr.PubKeyFromHex(tag[1])
		if err != nil {
			continue
		}
		if s := r.by[pk]; s != nil {
			s.wrap(now, r.watching[pk] > 0)
			r.save(pk, s)
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.subs[ws] == nil {
		r.subs[ws] = map[int][]nostr.PubKey{}
	}
	r.subs[ws][ssid] = citizens
	for _, pk := range citizens {
		r.watching[pk]++
		if s := r.by[pk]; s != nil {
			s.read(now)
			r.save(pk, s)
		}
	}
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
	}
	for _, pk := range citizens {
		if r.watching[pk]--; r.watching[pk] <= 0 {
			delete(r.watching, pk)
		}
	}
}

// Run rings the citizens that are due, until ctx ends.
func (r *Rings) Run(ctx context.Context) {
	ticker := time.NewTicker(tickEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			r.tick(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// tick starts each ring that is due, and removes each abandoned
// registration. The returned group ends when the rings end.
func (r *Rings) tick(ctx context.Context) *sync.WaitGroup {
	now := r.cfg.Now()
	var wg sync.WaitGroup
	r.mu.Lock()
	defer r.mu.Unlock()
	for pk, s := range r.by {
		if s.abandoned(now) {
			delete(r.by, pk)
			r.save(pk, nil)
			r.cfg.Logger.Info("rings: removed a registration after 7 days with no 2xx answer")
			continue
		}
		if r.busy[pk] || !s.due(now) || !r.spend(now) {
			continue
		}
		s.try(now)
		r.save(pk, s)
		r.busy[pk] = true
		snapshot := *s
		wg.Go(func() {
			status, err := r.ring(ctx, pk, snapshot, snapshot.Since)
			// An error can name the host of the URL, so the log says only
			// that the ring failed.
			r.cfg.Logger.Info("ring", "status", status, "failed", err != nil)
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.busy, pk)
			if s := r.by[pk]; s != nil {
				s.answer(status, r.cfg.Now())
				r.save(pk, s)
			}
		})
	}
	return &wg
}

// spend takes one ring from the cap of the relay for this minute. The
// caller holds r.mu.
func (r *Rings) spend(now time.Time) bool {
	minute := now.Unix() / 60
	if minute != r.minute {
		r.minute, r.inMinute = minute, 0
	}
	if r.inMinute >= r.cfg.PerMinute {
		return false
	}
	r.inMinute++
	return true
}

// ring makes one call to the ring URL of a citizen, and returns the HTTP
// status, zero for no answer.
func (r *Rings) ring(ctx context.Context, pk nostr.PubKey, s state, since int64) (int, error) {
	key, err := r.open(s.SealedKey)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(call{Type: "arc.mail", Citizen: pk.Hex(), Relay: s.Relay, Since: since})
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	request.Header.Set("Authorization", "Bearer "+string(key))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "arc-relay-rings")
	response, err := r.client.Do(request)
	if err != nil {
		// The error of the client names the URL, which can hold a secret.
		if urlErr, ok := errors.AsType[*url.Error](err); ok {
			err = urlErr.Err
		}
		return 0, err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, answerBytes))
	_ = response.Body.Close()
	return response.StatusCode, nil
}

// newClient makes the HTTP client of the rings. It follows no redirect,
// uses no proxy, and dials only an address that passed the checks.
func (r *Rings) newClient() *http.Client {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         r.dial,
		TLSClientConfig:     r.cfg.TLS,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: callTimeout,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       callTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dial resolves the host again, refuses it if any of its addresses is not
// public, and connects to a checked address.
func (r *Rings) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := r.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, addr := range addrs {
		target := net.JoinHostPort(addr.String(), port)
		var conn net.Conn
		if r.cfg.Dial != nil {
			conn, err = r.cfg.Dial(ctx, network, target)
		} else {
			dialer := net.Dialer{Timeout: callTimeout, Control: r.control}
			conn, err = dialer.DialContext(ctx, network, target)
		}
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// control checks the address that a connection goes to.
func (r *Rings) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if !r.public(ap.Addr()) {
		return fmt.Errorf("%s is not a public address", ap.Addr())
	}
	return nil
}

// resolve returns the addresses of host, or an error if one is not public.
func (r *Rings) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !r.public(addr) {
			return nil, fmt.Errorf("%s is not a public address", addr)
		}
		return []netip.Addr{addr}, nil
	}
	addrs, err := r.cfg.Lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("the host %s does not resolve", host)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("the host %s has no address", host)
	}
	for _, addr := range addrs {
		if !r.public(addr) {
			return nil, fmt.Errorf("the host %s resolves to %s, which is not a public address", host, addr.Unmap())
		}
	}
	return addrs, nil
}

// notPublic are prefixes that netip does not mark, and that do not reach
// a public server: this network, shared address space, protocol
// assignments, benchmarking, reserved and broadcast, NAT64 and 6to4.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
}

// public says whether a ring can go to addr.
func (r *Rings) public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	if slices.ContainsFunc(notPublic, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return false
	}
	return !r.own[addr]
}

// ownAddrs are the addresses of the relay: those of its interfaces, and
// the public addresses that the operator named.
func ownAddrs(named []netip.Addr) map[netip.Addr]bool {
	own := map[netip.Addr]bool{}
	for _, addr := range named {
		own[addr.Unmap()] = true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil {
			own[prefix.Addr().Unmap()] = true
		}
	}
	return own
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

// ServeHTTP answers GET, PUT and DELETE on Path, and POST on TestPath.
func (r *Rings) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	allowed := map[string][]string{
		Path:     {http.MethodGet, http.MethodPut, http.MethodDelete},
		TestPath: {http.MethodPost},
	}[req.URL.Path]
	if allowed == nil {
		http.NotFound(w, req)
		return
	}
	if !slices.Contains(allowed, req.Method) {
		w.Header().Set("Allow", fmt.Sprint(allowed))
		fail(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var body []byte
	if req.Method == http.MethodPut {
		var err error
		if body, err = io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody)); err != nil {
			fail(w, http.StatusRequestEntityTooLarge, "the body is too large")
			return
		}
	}
	citizen, relay, err := r.authenticate(req, body)
	if err != nil {
		fail(w, http.StatusUnauthorized, err.Error())
		return
	}
	switch req.Method {
	case http.MethodGet:
		r.show(w, citizen)
	case http.MethodPut:
		r.register(w, req.Context(), citizen, relay, body)
	case http.MethodDelete:
		r.mu.Lock()
		delete(r.by, citizen)
		r.save(citizen, nil)
		r.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		r.test(w, req.Context(), citizen)
	}
}

// authenticate checks the NIP-98 header and returns the citizen of the
// request, and the wss URL of this relay as the request names it.
func (r *Rings) authenticate(req *http.Request, body []byte) (nostr.PubKey, string, error) {
	now := r.cfg.Now()
	event, err := turn.VerifyAuthBody(req.Header.Get("Authorization"), req.Method, req.Host, req.URL.Path, body, now)
	if err != nil {
		return nostr.PubKey{}, "", err
	}
	citizen := event.PubKey
	if encoded := req.Header.Get(TokenHeader); encoded != "" {
		data, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nostr.PubKey{}, "", errors.New("the token is not base64")
		}
		var token nostr.Event
		if err := json.Unmarshal(data, &token); err != nil {
			return nostr.PubKey{}, "", errors.New("the token is not an event")
		}
		if citizen, _, err = devices.CheckToken(token, event.PubKey, nostr.Timestamp(now.Unix())); err != nil {
			return nostr.PubKey{}, "", err
		}
	}
	u, _ := url.Parse(event.Tags.Find("u")[1])
	scheme := "wss"
	if u.Scheme == "http" {
		scheme = "ws"
	}
	return citizen, scheme + "://" + u.Host, nil
}

func (r *Rings) show(w http.ResponseWriter, citizen nostr.PubKey) {
	r.mu.Lock()
	s := r.by[citizen]
	var status Status
	if s != nil {
		status = r.status(citizen, s)
	}
	r.mu.Unlock()
	if s == nil {
		fail(w, http.StatusNotFound, errNoRegistration)
		return
	}
	answer(w, http.StatusOK, status)
}

// status is the registration as GET shows it. The caller holds r.mu.
func (r *Rings) status(citizen nostr.PubKey, s *state) Status {
	return Status{Settings: s.Settings, Citizen: citizen.Hex(), LastRing: s.LastRing, LastStatus: s.LastStatus, Left: s.left(r.cfg.Now())}
}

// register checks and keeps a registration. A new registration keeps the
// count of the day and the time of the last ring, so that it cannot reset
// the caps.
func (r *Rings) register(w http.ResponseWriter, ctx context.Context, citizen nostr.PubKey, relay string, body []byte) {
	var request Request
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		fail(w, http.StatusBadRequest, "the body is not a registration: "+err.Error())
		return
	}
	if err := r.check(ctx, &request); err != nil {
		fail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	sealed, err := r.seal(request.Key)
	if err != nil {
		fail(w, http.StatusInternalServerError, "the relay cannot keep the key")
		return
	}
	now := r.cfg.Now()
	s := &state{Settings: request.Settings, SealedKey: sealed, Relay: relay, Created: now.Unix(), LastRead: now.Unix()}
	r.mu.Lock()
	if old := r.by[citizen]; old != nil {
		s.LastRing, s.LastStatus, s.Day, s.Used = old.LastRing, old.LastStatus, old.Day, old.Used
	}
	r.by[citizen] = s
	r.save(citizen, s)
	status := r.status(citizen, s)
	r.mu.Unlock()
	answer(w, http.StatusOK, status)
}

// check applies the rules of a registration, and fills in the defaults.
func (r *Rings) check(ctx context.Context, request *Request) error {
	if n := len(request.Key); n < MinKey || n > MaxKey {
		return fmt.Errorf("the key holds %d bytes; it must hold %d to %d", n, MinKey, MaxKey)
	}
	switch interval := request.MinInterval; {
	case interval == 0:
		request.MinInterval = DefaultInterval
	case interval < MinInterval || interval > MaxInterval:
		return fmt.Errorf("min_interval is %d; it must be %d to %d seconds", interval, MinInterval, MaxInterval)
	}
	switch c := request.DailyCap; {
	case c == 0:
		request.DailyCap = min(DefaultDailyCap, r.cfg.DailyCap)
	case c < 1 || c > r.cfg.DailyCap:
		return fmt.Errorf("daily_cap is %d; this relay takes 1 to %d", c, r.cfg.DailyCap)
	}
	switch request.RingOn {
	case "":
		request.RingOn = RingOnAny
	case RingOnAny:
	case RingOnStamped:
		return errors.New(errStampedNotBuilt)
	default:
		return fmt.Errorf("ring_on is %q; it must be %q or %q", request.RingOn, RingOnAny, RingOnStamped)
	}
	return r.checkURL(ctx, request.URL)
}

// checkURL refuses a ring URL that is not public https on an allowed port.
func (r *Rings) checkURL(ctx context.Context, raw string) error {
	if len(raw) > maxURL {
		return fmt.Errorf("the URL holds more than %d bytes", maxURL)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("the url must be an https URL with a host, and no user or fragment")
	}
	port := defaultPort
	if p := u.Port(); p != "" {
		if port, err = strconv.Atoi(p); err != nil {
			return fmt.Errorf("the port %q is not a number", p)
		}
	}
	if !slices.Contains(r.cfg.Ports, port) {
		return fmt.Errorf("the port %d is not one that this relay rings: %v", port, r.cfg.Ports)
	}
	_, err = r.resolve(ctx, u.Hostname())
	return err
}

// test rings once now. It counts against the cap of the day and of the
// relay, and plans no retry.
func (r *Rings) test(w http.ResponseWriter, ctx context.Context, citizen nostr.PubKey) {
	now := r.cfg.Now()
	r.mu.Lock()
	s := r.by[citizen]
	switch {
	case s == nil:
		r.mu.Unlock()
		fail(w, http.StatusNotFound, errNoRegistration)
		return
	case s.left(now) == 0:
		r.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "the rings of today are used")
		return
	case r.busy[citizen] || !r.spend(now):
		r.mu.Unlock()
		fail(w, http.StatusTooManyRequests, "the relay rings too much now; try again in a minute")
		return
	}
	if s.Day != day(now) {
		s.Day, s.Used = day(now), 0
	}
	s.Used++
	s.LastRing = now.Unix()
	r.save(citizen, s)
	r.busy[citizen] = true
	snapshot := *s
	r.mu.Unlock()

	since := snapshot.Since
	if since == 0 {
		since = now.Unix()
	}
	status, err := r.ring(ctx, citizen, snapshot, since)
	r.mu.Lock()
	delete(r.busy, citizen)
	if s := r.by[citizen]; s != nil {
		s.LastStatus = status
		r.save(citizen, s)
	}
	r.mu.Unlock()
	answer(w, http.StatusOK, TestAnswer{Status: status, Error: errString(err)})
}

func answer(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, code int, message string) {
	answer(w, code, map[string]string{"error": message})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
