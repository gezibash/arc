package direct

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/canonical"
)

// Two citizens that already speak over a relay may carry the rest of one
// conversation between themselves.
//
// The relay carries the whole negotiation. One side offers, the other
// accepts, and each names an address that its owner allowed. Both sides then
// try to reach one another. The side that offered picks the connection that
// stood up first, and the other side agrees. From then on the packets of
// that conversation travel the carrier, and the relay sees none of them.
//
// A route lives for its lease and no longer. Either side withdraws at any
// time, and the conversation returns to the relay.
const (
	// Topic names the control messages of a direct route inside an event
	// frame.
	Topic = "arc.direct.v1"
	// BindingDomain stands at the front of the value that ties a carrier to
	// one conversation.
	BindingDomain = "ARC_DIRECT_V1"
	// AttemptLimit bounds one negotiation.
	AttemptLimit = 3 * time.Second
	// MaxRoutes is how many routes one citizen holds.
	MaxRoutes = 32
)

// The phases of a route.
const (
	phaseOffered   = "offered"
	phaseProbing   = "probing"
	phasePreparing = "preparing"
	phaseActive    = "active"
)

// Manager holds the direct routes of one citizen.
type Manager struct {
	me    *identity.Identity
	rules []Rule
	log   *slog.Logger

	// control carries one control message to a peer over the relay.
	control func(peer []byte, body []byte) error

	mu     sync.Mutex
	routes map[string]*Route
}

// Route is one conversation that may leave the relay.
type Route struct {
	ID    string
	Peer  []byte
	Role  string
	Scope map[string]any

	rule            *Rule
	credentials     *Credentials
	peerFingerprint []byte
	relaySID        []byte
	binding         []byte
	leaseMS         int
	deadline        time.Time

	phase    string
	listener *Listener
	standing map[string]*Conn
	selected string
	carrier  *Conn
	ready    chan error
}

// NewManager builds the manager of one citizen. The control function sends
// one message to a peer over the relay.
func NewManager(me *identity.Identity, rules []Rule, control func(peer, body []byte) error, log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{me: me, rules: rules, log: log, control: control, routes: map[string]*Route{}}
}

// Allowed says whether the owner of this citizen lets one conversation leave
// the relay.
func (m *Manager) Allowed(peer []byte, capability, scheme, path string) bool {
	return FindRule(m.rules, peer, capability, scheme, path) != nil
}

// Carrier returns the carrier of an active route, and nothing while the
// conversation stays on the relay.
func (m *Manager) Carrier(peer []byte, capability, scheme, path string) *Conn {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, route := range m.routes {
		if route.phase != phaseActive || string(route.Peer) != string(peer) {
			continue
		}
		if route.deadline.Before(time.Now()) {
			continue
		}
		if route.Scope["capability"] == capability && route.Scope["scheme"] == scheme &&
			route.Scope["path"] == path {
			return route.carrier
		}
	}
	return nil
}

// CarrierFor returns the carrier that stands with one peer, whatever the
// conversation. The side that answers an offer knows the peer, and reads the
// scope from the route itself.
func (m *Manager) CarrierFor(peer []byte) *Conn {
	m.mu.Lock()
	defer m.mu.Unlock()

	for _, route := range m.routes {
		if route.phase == phaseActive && string(route.Peer) == string(peer) &&
			route.deadline.After(time.Now()) {
			return route.carrier
		}
	}
	return nil
}

// Offer asks a peer to carry one conversation off the relay. It returns when
// the route stands, or when the attempt runs out.
//
// The scope names what the conversation is: the capability, the scheme, the
// path, the hash of the signed package, the method, and the session of the
// relay. Both sides sign the same scope into the binding of the carrier.
func (m *Manager) Offer(ctx context.Context, peer []byte, scope map[string]any, relaySID []byte) (*Route, error) {
	capability, _ := scope["capability"].(string)
	scheme, _ := scope["scheme"].(string)
	path, _ := scope["path"].(string)

	rule := FindRule(m.rules, peer, capability, scheme, path)
	if rule == nil {
		return nil, ErrNoRule
	}

	m.mu.Lock()
	if len(m.routes) >= MaxRoutes {
		m.mu.Unlock()
		return nil, fmt.Errorf("direct: this citizen holds %d routes already", MaxRoutes)
	}
	m.mu.Unlock()

	credentials, err := NewCredentials()
	if err != nil {
		return nil, err
	}

	id, err := randomID()
	if err != nil {
		return nil, err
	}

	route := &Route{
		ID: id, Peer: peer, Role: "caller", Scope: scope,
		rule: rule, credentials: credentials, relaySID: relaySID,
		leaseMS: rule.LeaseMS, deadline: time.Now().Add(AttemptLimit),
		phase: phaseOffered, standing: map[string]*Conn{}, ready: make(chan error, 1),
	}

	m.mu.Lock()
	m.routes[id] = route
	m.mu.Unlock()

	if err := m.send(route, map[string]any{
		"type": "offer", "scope": scope,
		"fingerprint": hex.EncodeToString(credentials.Fingerprint),
		"lease_ms":    rule.LeaseMS, "hole_punch": false,
	}); err != nil {
		m.retire(id, "the offer did not reach the peer")
		return nil, err
	}

	select {
	case err := <-route.ready:
		if err != nil {
			return nil, err
		}
		return route, nil
	case <-time.After(AttemptLimit):
		m.retire(id, "the peer did not finish the negotiation")
		return nil, fmt.Errorf("direct: the route did not stand in time")
	case <-ctx.Done():
		m.retire(id, "the caller stopped waiting")
		return nil, ctx.Err()
	}
}

// Control takes one control message that arrived over the relay.
func (m *Manager) Control(peer, relaySID, body []byte) {
	value, err := canonical.Decode(body)
	if err != nil {
		return
	}

	message, ok := value.(map[string]any)
	if !ok {
		return
	}

	id, _ := message["id"].(string)
	kind, _ := message["type"].(string)

	if kind == "offer" {
		m.receiveOffer(peer, relaySID, id, message)
		return
	}

	m.mu.Lock()
	route := m.routes[id]
	m.mu.Unlock()

	if route == nil || string(route.Peer) != string(peer) {
		return
	}

	switch kind {
	case "accept":
		m.receiveAccept(route, message)
	case "candidates":
		m.receiveCandidates(route, message)
	case "nominate":
		m.receiveNominate(route, message)
	case "ready":
		m.receiveReady(route, message)
	case "withdraw":
		m.retire(route.ID, "the peer withdrew")
	}
}

// Withdraw ends one route, and tells the peer.
func (m *Manager) Withdraw(id string) {
	m.mu.Lock()
	route := m.routes[id]
	m.mu.Unlock()

	if route != nil {
		m.send(route, map[string]any{"type": "withdraw"})
	}
	m.retire(id, "withdrawn")
}

// Close ends every route of this citizen.
func (m *Manager) Close() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.routes))
	for id := range m.routes {
		ids = append(ids, id)
	}
	m.mu.Unlock()

	for _, id := range ids {
		m.retire(id, "the citizen is stopping")
	}
}

// -- the side that answers ---------------------------------------------------

// receiveOffer answers the offer of a peer, when the owner allows it.
func (m *Manager) receiveOffer(peer, relaySID []byte, id string, message map[string]any) {
	scope, _ := message["scope"].(map[string]any)
	if scope == nil || !validID(id) {
		return
	}

	capability, _ := scope["capability"].(string)
	scheme, _ := scope["scheme"].(string)
	path, _ := scope["path"].(string)

	rule := FindRule(m.rules, peer, capability, scheme, path)
	if rule == nil {
		return
	}

	fingerprint, err := hex.DecodeString(text(message["fingerprint"]))
	if err != nil || len(fingerprint) != 32 {
		return
	}

	lease, ok := wholeNumber(message["lease_ms"])
	if !ok || lease < MinLeaseMS || lease > rule.LeaseMS {
		return
	}

	credentials, err := NewCredentials()
	if err != nil {
		return
	}

	m.mu.Lock()
	if _, twice := m.routes[id]; twice || len(m.routes) >= MaxRoutes {
		m.mu.Unlock()
		return
	}

	route := &Route{
		ID: id, Peer: peer, Role: "provider", Scope: scope,
		rule: rule, credentials: credentials, peerFingerprint: fingerprint,
		relaySID: relaySID, leaseMS: lease, deadline: time.Now().Add(AttemptLimit),
		phase: phaseProbing, standing: map[string]*Conn{}, ready: make(chan error, 1),
	}
	route.binding = route.bindingValue(m.me)
	m.routes[id] = route
	options := route.options(m.me, route.Role)
	m.mu.Unlock()

	candidate := m.listen(route, options)
	if err := m.send(route, map[string]any{
		"type": "accept", "lease_ms": lease,
		"fingerprint": hex.EncodeToString(credentials.Fingerprint),
		"candidate":   candidate, "hole_punch": false,
	}); err != nil {
		m.retire(id, "the answer did not reach the peer")
	}
}

// receiveCandidates dials the address that the other side named.
func (m *Manager) receiveCandidates(route *Route, message map[string]any) {
	m.mu.Lock()
	ready := route.Role == "provider" && route.phase == phaseProbing
	options := route.options(m.me, "caller")
	m.mu.Unlock()

	if !ready {
		return
	}

	if candidate, ok := message["candidate"].(map[string]any); ok {
		m.dial(route, options, candidate)
	}
}

// receiveNominate takes the connection that the other side picked. The
// nomination may arrive before that connection stands, so it is remembered
// and the route finishes when the connection arrives.
func (m *Manager) receiveNominate(route *Route, message map[string]any) {
	direction, _ := message["direction"].(string)
	if direction != "caller" && direction != "provider" {
		return
	}

	m.mu.Lock()
	if route.Role != "provider" || route.phase != phaseProbing {
		m.mu.Unlock()
		return
	}
	route.selected = direction
	_, standing := route.standing[direction]
	m.mu.Unlock()

	if standing {
		m.finish(route, direction)
	}
}

// finish takes the connection that both sides agreed, closes the rest, and
// tells the other side that the route stands.
func (m *Manager) finish(route *Route, direction string) {
	m.mu.Lock()
	if route.phase != phaseProbing {
		m.mu.Unlock()
		return
	}

	carrier, standing := route.standing[direction]
	if !standing {
		m.mu.Unlock()
		return
	}

	route.carrier = carrier
	route.phase = phaseActive
	route.deadline = time.Now().Add(time.Duration(route.leaseMS) * time.Millisecond)

	for name, held := range route.standing {
		if name != direction {
			held.Close()
			delete(route.standing, name)
		}
	}
	m.mu.Unlock()

	m.closeListener(route)
	if err := m.send(route, map[string]any{"type": "ready", "direction": direction}); err != nil {
		m.retire(route.ID, "the answer did not reach the peer")
	}
}

// -- the side that offered ---------------------------------------------------

// receiveAccept reads the answer of the peer, and starts to reach it.
func (m *Manager) receiveAccept(route *Route, message map[string]any) {
	fingerprint, err := hex.DecodeString(text(message["fingerprint"]))
	if err != nil || len(fingerprint) != 32 {
		m.retire(route.ID, "the peer named no certificate")
		return
	}

	m.mu.Lock()
	if route.Role != "caller" || route.phase != phaseOffered {
		m.mu.Unlock()
		return
	}

	lease, ok := wholeNumber(message["lease_ms"])
	if !ok || lease < MinLeaseMS || lease > route.leaseMS {
		m.mu.Unlock()
		m.retire(route.ID, "the peer named a lease that is not allowed")
		return
	}

	route.peerFingerprint = fingerprint
	route.leaseMS = lease
	route.phase = phaseProbing
	route.binding = route.bindingValue(m.me)

	mine := route.options(m.me, route.Role)
	theirs := route.options(m.me, "provider")
	m.mu.Unlock()

	candidate := m.listen(route, mine)

	if given, ok := message["candidate"].(map[string]any); ok {
		m.dial(route, theirs, given)
	}

	if err := m.send(route, map[string]any{"type": "candidates", "candidate": candidate}); err != nil {
		m.retire(route.ID, "the candidate did not reach the peer")
	}
}

// receiveReady finishes the negotiation on the side that offered.
func (m *Manager) receiveReady(route *Route, message map[string]any) {
	direction, _ := message["direction"].(string)

	m.mu.Lock()
	if route.Role != "caller" || route.phase != phasePreparing {
		m.mu.Unlock()
		return
	}
	if direction != route.selected {
		m.mu.Unlock()
		m.retire(route.ID, "the peer agreed to another connection")
		return
	}

	route.phase = phaseActive
	route.deadline = time.Now().Add(time.Duration(route.leaseMS) * time.Millisecond)
	m.mu.Unlock()

	select {
	case route.ready <- nil:
	default:
	}
}

// -- reaching the other side -------------------------------------------------

// listen waits for the peer where the owner allowed, and returns the address
// to send it.
func (m *Manager) listen(route *Route, options Options) any {
	if route.rule.Listen == nil {
		return nil
	}

	held := route.rule.Listen
	address := netip.AddrPortFrom(held.Bind, uint16(held.Port)).String()

	listener, err := Listen(address, options)
	if err != nil {
		m.log.Debug("the direct listener did not start", "error", err)
		return nil
	}

	m.mu.Lock()
	route.listener = listener
	m.mu.Unlock()

	go m.accept(route, listener, options)

	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		return nil
	}
	number, _ := strconv.Atoi(port)

	return map[string]any{"host": held.Address.String(), "port": number}
}

// accept takes the peer when it arrives.
func (m *Manager) accept(route *Route, listener *Listener, options Options) {
	ctx, cancel := context.WithTimeout(context.Background(), AttemptLimit)
	defer cancel()

	carrier, err := listener.Accept(ctx)
	if err != nil {
		return
	}
	m.standing(route, route.Role, carrier)
}

// dial reaches the address that the peer named, when the owner allows it.
func (m *Manager) dial(route *Route, options Options, candidate map[string]any) {
	address, err := route.rule.Candidate(candidate)
	if err != nil {
		m.log.Debug("the address of the peer is not allowed", "error", err)
		return
	}

	direction := options.direction

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), AttemptLimit)
		defer cancel()

		carrier, err := Dial(ctx, address, options)
		if err != nil {
			m.log.Debug("the direct dial failed", "address", address, "error", err)
			return
		}
		m.standing(route, direction, carrier)
	}()
}

// standing takes a connection that stood up. The side that offered picks the
// first one and tells the other side.
func (m *Manager) standing(route *Route, direction string, carrier *Conn) {
	m.mu.Lock()

	if route.phase != phaseProbing {
		m.mu.Unlock()
		carrier.Close()
		return
	}

	route.standing[direction] = carrier

	// The side that answers waits for the nomination of the other side.
	if route.Role != "caller" {
		nominated := route.selected == direction
		m.mu.Unlock()

		if nominated {
			m.finish(route, direction)
		}
		return
	}

	if route.selected != "" {
		m.mu.Unlock()
		return
	}

	route.selected = direction
	route.carrier = carrier
	route.phase = phasePreparing

	for name, held := range route.standing {
		if name != direction {
			held.Close()
			delete(route.standing, name)
		}
	}
	m.mu.Unlock()

	m.closeListener(route)
	if err := m.send(route, map[string]any{"type": "nominate", "direction": direction}); err != nil {
		m.retire(route.ID, "the nomination did not reach the peer")
	}
}

func (m *Manager) closeListener(route *Route) {
	m.mu.Lock()
	listener := route.listener
	route.listener = nil
	m.mu.Unlock()

	if listener != nil {
		listener.Close()
	}
}

// -- the shape of a route ----------------------------------------------------

// options are what the carrier of one direction needs.
func (r *Route) options(me *identity.Identity, direction string) Options {
	digest := sha256.Sum256(append(append([]byte{}, r.binding...), direction...))

	return Options{
		Identity: me, Credentials: r.credentials,
		PeerKey: r.Peer, PeerFingerprint: r.peerFingerprint,
		Binding: digest[:], Timeout: AttemptLimit,
		direction: direction,
	}
}

// bindingValue ties a carrier to one conversation: the route, the session of
// the relay, the scope, both certificates, and the lease. Both sides reach
// the same value, and a carrier of another conversation never matches it.
func (r *Route) bindingValue(me *identity.Identity) []byte {
	callerFingerprint, providerFingerprint := r.credentials.Fingerprint, r.peerFingerprint
	if r.Role == "provider" {
		callerFingerprint, providerFingerprint = r.peerFingerprint, r.credentials.Fingerprint
	}

	fields := make([]any, 0, 10)
	for _, name := range []string{
		"version", "caller", "provider", "capability", "scheme", "path",
		"package_hash", "method", "session", "ek",
	} {
		fields = append(fields, r.Scope[name])
	}

	encoded, err := canonical.Encode([]any{
		r.ID, hex.EncodeToString(r.relaySID), fields,
		hex.EncodeToString(callerFingerprint), hex.EncodeToString(providerFingerprint),
		r.leaseMS,
	})
	if err != nil {
		return nil
	}

	digest := sha256.Sum256(append([]byte(BindingDomain), encoded...))
	return digest[:]
}

// send writes one control message to the peer over the relay.
func (m *Manager) send(route *Route, message map[string]any) error {
	fields := map[string]any{"id": route.ID}
	for name, value := range message {
		fields[name] = value
	}

	body, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	return m.control(route.Peer, body)
}

// retire ends one route and everything it holds.
func (m *Manager) retire(id, reason string) {
	m.mu.Lock()
	route := m.routes[id]
	delete(m.routes, id)

	var listener *Listener
	var standing []*Conn

	if route != nil {
		listener = route.listener
		route.listener = nil
		for name, held := range route.standing {
			standing = append(standing, held)
			delete(route.standing, name)
		}
	}
	m.mu.Unlock()

	if route == nil {
		return
	}

	m.log.Debug("the direct route ended", "route", id, "reason", reason)

	if listener != nil {
		listener.Close()
	}
	for _, held := range standing {
		held.Close()
	}

	select {
	case route.ready <- fmt.Errorf("direct: %s", reason):
	default:
	}
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

func validID(id string) bool {
	raw, err := hex.DecodeString(id)
	return err == nil && len(raw) == 16
}

func text(value any) string {
	out, _ := value.(string)
	return out
}
