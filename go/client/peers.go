package client

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gezibash/arc/go/capability"
	"github.com/gezibash/arc/go/direct"
	"github.com/gezibash/arc/go/frame"
	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/packet"
	"github.com/gezibash/arc/go/session"
)

// The limits of one request.
const (
	// DefaultRequestTimeout is how long a caller waits for a reply.
	DefaultRequestTimeout = 10 * time.Second
	// MaxBodyBytes caps one request body.
	MaxBodyBytes = 1024 * 1024
	// MaxManifestBytes caps one manifest answer.
	MaxManifestBytes = 256 * 1024
)

// ErrNoAnswer reports a request that the peer did not answer in time. The
// outcome of that request is unknown, and ARC never sends it again.
var ErrNoAnswer = errors.New("client: the peer did not answer")

// RemoteError reports an error frame from the peer.
type RemoteError struct {
	Code    string
	Message string
}

func (e *RemoteError) Error() string {
	if e.Message == "" {
		return "client: the peer answered " + e.Code
	}
	return fmt.Sprintf("client: the peer answered %s: %s", e.Code, e.Message)
}

var addressPattern = regexp.MustCompile(`^([a-z][a-z0-9-]*)\+arc$`)

// Address is one capability address:
//
//	<scheme>+arc://<64 characters of hex>/<resource>
type Address struct {
	Scheme string
	Key    []byte
	Path   string
}

// ParseAddress reads one capability address.
func ParseAddress(raw string) (*Address, error) {
	invalid := errors.New("client: the address must be <scheme>+arc://<public key>/<path>")
	if len(raw) > 4096 {
		return nil, invalid
	}

	parsed, err := url.Parse(raw)
	if err != nil {
		return nil, invalid
	}

	scheme := addressPattern.FindStringSubmatch(parsed.Scheme)
	if scheme == nil || parsed.User != nil || parsed.Port() != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, invalid
	}

	key, err := hex.DecodeString(strings.ToLower(parsed.Host))
	if err != nil || len(key) != identity.SeedBytes {
		return nil, invalid
	}

	path := parsed.Path
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return nil, invalid
	}
	return &Address{Scheme: scheme[1], Key: key, Path: path}, nil
}

// Peers carries requests to other citizens over one relay connection. It
// takes over the packet channel of the client, so a program uses either
// Peers or Packets, and never both.
type Peers struct {
	client *Client
	me     *identity.Identity

	direct *direct.Manager

	mu       sync.Mutex
	sessions map[string]*session.Session
	waiting  map[string]chan *frame.Frame
	carriers map[string]*direct.Conn

	events chan *Event
	once   sync.Once
}

// Event is a frame that arrives without a request, for example a message
// from a provider.
type Event struct {
	From  []byte
	Frame *frame.Frame
}

// Peers starts the multiplexer. Call it once for one client.
func (c *Client) Peers() *Peers {
	peers := &Peers{
		client:   c,
		me:       c.me,
		sessions: map[string]*session.Session{},
		waiting:  map[string]chan *frame.Frame{},
		carriers: map[string]*direct.Conn{},
		events:   make(chan *Event, 32),
	}

	go peers.read()
	return peers
}

// Events returns the frames that arrive without a request.
func (p *Peers) Events() <-chan *Event { return p.events }

// Direct gives this caller the rules of its owner, so a conversation may
// leave the relay. Without them every packet travels the relay.
func (p *Peers) Direct(rules []direct.Rule, log *slog.Logger) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.direct = direct.NewManager(p.me, rules, p.sendControl, log)
}

// Promote asks a peer to carry one conversation off the relay. The signed
// package of the capability names what the conversation is, and both sides
// sign the same scope into the binding of the carrier.
func (p *Peers) Promote(ctx context.Context, address string, capabilityID string) error {
	p.mu.Lock()
	manager := p.direct
	p.mu.Unlock()

	if manager == nil {
		return direct.ErrNoRule
	}

	target, err := ParseAddress(address)
	if err != nil {
		return err
	}
	if capabilityID == "" {
		capabilityID = "primary"
	}

	pkg, err := p.Detail(ctx, target.Key, capabilityID)
	if err != nil {
		return err
	}

	fields, _ := pkg["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)

	talk, err := p.session(target.Key)
	if err != nil {
		return err
	}

	scope := map[string]any{
		"version":      1,
		"caller":       p.me.EncodePublicKey(),
		"provider":     hex.EncodeToString(target.Key),
		"capability":   capabilityID,
		"scheme":       target.Scheme,
		"path":         target.Path,
		"package_hash": pkg["package_hash"],
		"method":       invocation["method"],
		"session":      hex.EncodeToString(talk.ID),
		"ek":           hex.EncodeToString(talk.EphemeralPublic),
	}

	route, err := manager.Offer(ctx, target.Key, scope, talk.ID)
	if err != nil {
		return err
	}

	carrier := manager.Carrier(target.Key, capabilityID, target.Scheme, target.Path)
	if carrier == nil {
		return direct.ErrClosed
	}

	p.mu.Lock()
	p.carriers[string(target.Key)] = carrier
	p.mu.Unlock()

	go p.readCarrier(target.Key, carrier)

	_ = route
	return nil
}

// sendControl carries one message of a direct route to a peer over the
// relay, as an event of its own.
func (p *Peers) sendControl(peer, body []byte) error {
	event, err := frame.EncodeEvent(direct.Topic, body, nil)
	if err != nil {
		return err
	}
	return p.send(peer, event)
}

// readCarrier reads the packets of a carrier, and answers them as if they
// had come over the relay.
func (p *Peers) readCarrier(peer []byte, carrier *direct.Conn) {
	defer func() {
		p.mu.Lock()
		if p.carriers[string(peer)] == carrier {
			delete(p.carriers, string(peer))
		}
		p.mu.Unlock()
	}()

	for raw := range carrier.Packets() {
		p.handlePacket(raw)
	}
}

// Request sends one request to a peer, and waits for the answer.
func (p *Peers) Request(ctx context.Context, peer []byte, meta map[string]any, body []byte) (*frame.Frame, error) {
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("client: the body is over %d bytes", MaxBodyBytes)
	}

	requestID := frame.NewRequestID()
	request, err := frame.Encode(frame.Request, requestID, meta, body)
	if err != nil {
		return nil, err
	}

	answers := make(chan *frame.Frame, 1)
	key := hex.EncodeToString(requestID)

	p.mu.Lock()
	p.waiting[key] = answers
	p.mu.Unlock()

	defer func() {
		p.mu.Lock()
		delete(p.waiting, key)
		p.mu.Unlock()
	}()

	if err := p.send(peer, request); err != nil {
		return nil, err
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultRequestTimeout)
		defer cancel()
	}

	// A conversation that left the relay does not end when the relay
	// connection does, so only a request on the relay watches it.
	p.mu.Lock()
	onRelay := p.carriers[string(peer)] == nil
	p.mu.Unlock()

	var relayEnded <-chan struct{}
	if onRelay {
		relayEnded = p.client.Done()
	}

	select {
	case answer := <-answers:
		if answer.Type == frame.Error {
			return answer, &RemoteError{Code: answer.Code(), Message: answer.Message()}
		}
		return answer, nil
	case <-ctx.Done():
		return nil, ErrNoAnswer
	case <-relayEnded:
		return nil, p.client.Err()
	}
}

// SendFrame sends one frame to a peer, and waits for nothing. An event
// travels this way.
func (p *Peers) SendFrame(peer, body []byte) error {
	return p.send(peer, body)
}

// Manifest asks a peer for the summary of what it offers.
func (p *Peers) Manifest(ctx context.Context, peer []byte) (map[string]any, error) {
	return p.document(ctx, peer, capability.SummaryPath)
}

// Detail asks a peer for the signed package of one capability, and verifies
// that the peer signed it.
func (p *Peers) Detail(ctx context.Context, peer []byte, id string) (map[string]any, error) {
	document, err := p.document(ctx, peer, capability.DetailPath(id))
	if err != nil {
		return nil, err
	}

	verified, err := capability.Verify(document)
	if err != nil {
		return nil, err
	}

	provider, _ := verified["provider"].(map[string]any)
	if key, _ := provider["public_key"].(string); key != hex.EncodeToString(peer) {
		return nil, capability.ErrSignerMismatch
	}
	return verified, nil
}

// Call resolves the capability of an address, and sends one request under it.
// The signed package names the method, and the address names the path.
func (p *Peers) Call(ctx context.Context, address string, body []byte, capabilityID string) (*frame.Frame, error) {
	target, err := ParseAddress(address)
	if err != nil {
		return nil, err
	}
	if capabilityID == "" {
		capabilityID = "primary"
	}

	pkg, err := p.Detail(ctx, target.Key, capabilityID)
	if err != nil {
		return nil, err
	}

	fields, _ := pkg["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)
	scheme, _ := fields["scheme"].(string)
	method, _ := invocation["method"].(string)

	if scheme != target.Scheme {
		return nil, fmt.Errorf("client: the capability speaks %s, and the address says %s", scheme, target.Scheme)
	}

	return p.Request(ctx, target.Key, map[string]any{
		"method":        method,
		"path":          target.Path,
		"capability_id": capabilityID,
	}, body)
}

func (p *Peers) document(ctx context.Context, peer []byte, path string) (map[string]any, error) {
	answer, err := p.Request(ctx, peer, map[string]any{"method": "GET", "path": path}, nil)
	if err != nil {
		return nil, err
	}
	if len(answer.Body) > MaxManifestBytes {
		return nil, fmt.Errorf("client: the manifest is over %d bytes", MaxManifestBytes)
	}

	decoder := json.NewDecoder(strings.NewReader(string(answer.Body)))
	decoder.UseNumber()

	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, errors.New("client: the manifest is not JSON")
	}
	return document, nil
}

// send encrypts one frame to a peer. A conversation that left the relay
// travels its carrier, and every other one travels the relay.
func (p *Peers) send(peer, body []byte) error {
	talk, err := p.session(peer)
	if err != nil {
		return err
	}

	nonce, ciphertext, seq, err := talk.Encrypt(body)
	if err != nil {
		return err
	}

	raw, err := packet.Encode(p.me, peer, talk.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		return err
	}

	p.mu.Lock()
	carrier := p.carriers[string(peer)]
	p.mu.Unlock()

	if carrier != nil {
		if err := carrier.Send(raw); err == nil {
			return nil
		}
	}
	return p.client.SendPacket(raw)
}

func (p *Peers) session(peer []byte) (*session.Session, error) {
	p.mu.Lock()
	held, ok := p.sessions[string(peer)]
	p.mu.Unlock()

	if ok {
		return held, nil
	}

	started, err := session.Establish(p.me, peer)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	p.sessions[string(peer)] = started
	p.mu.Unlock()
	return started, nil
}

// read decrypts every packet that arrives, and hands each frame to the
// request that waits for it.
func (p *Peers) read() {
	defer p.once.Do(func() { close(p.events) })

	for raw := range p.client.Packets() {
		p.handlePacket(raw)
	}
}

// handlePacket reads one packet, whichever way it arrived.
func (p *Peers) handlePacket(raw []byte) {
	{
		decoded, err := packet.Decode(raw)
		if err != nil {
			return
		}

		p.mu.Lock()
		talk, ok := p.sessions[string(decoded.Src)]
		manager := p.direct
		p.mu.Unlock()

		if !ok {
			// The peer spoke first. Join the session that it names.
			joined, err := session.Accept(p.me, decoded.Src, decoded.EphemeralPublic, decoded.SessionID)
			if err != nil {
				return
			}
			talk = joined

			p.mu.Lock()
			p.sessions[string(decoded.Src)] = joined
			p.mu.Unlock()
		}

		plaintext, err := talk.Decrypt(decoded.Nonce, decoded.Ciphertext)
		if err != nil {
			// A reply of another session, or another key. Join and try once.
			joined, joinErr := session.Accept(p.me, decoded.Src, decoded.EphemeralPublic, decoded.SessionID)
			if joinErr != nil {
				return
			}
			if plaintext, err = joined.Decrypt(decoded.Nonce, decoded.Ciphertext); err != nil {
				return
			}
		}

		message, err := frame.Decode(plaintext)
		if err != nil {
			return
		}

		// A peer that answers about a direct route says so in an event of
		// its own.
		if manager != nil && message.Type == frame.Event && message.Meta["topic"] == direct.Topic {
			manager.Control(decoded.Src, decoded.SessionID, message.Body)
			return
		}

		key := hex.EncodeToString(message.RequestID)

		p.mu.Lock()
		waiting := p.waiting[key]
		delete(p.waiting, key)
		p.mu.Unlock()

		if waiting != nil {
			waiting <- message
			return
		}

		select {
		case p.events <- &Event{From: decoded.Src, Frame: message}:
		default:
		}
	}
}
