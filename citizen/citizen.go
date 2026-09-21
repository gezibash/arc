package citizen

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/capability"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/direct"
	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/provider/host"
	"github.com/gezibash/arc/session"
)

// The timings of a serving citizen.
const (
	// AnnounceEvery is how often the citizen renews its announcement. The
	// record lives for 180 seconds.
	AnnounceEvery = 150 * time.Second
	// MaxSkew is how far the time of a packet may stand from the time here.
	MaxSkew = 120 * time.Second
	// MaxBodyBytes caps one request body, and one reply body that the
	// provider writes as base64.
	MaxBodyBytes = 1024 * 1024
	// MaxPending is how many requests the provider holds at one time. A
	// request over it fails before it reaches the provider.
	MaxPending = 256
)

// Options holds what a serving citizen needs.
type Options struct {
	// Identity is the identity that serves. It is needed.
	Identity *identity.Identity
	// Relay is the address of the relay, for example "127.0.0.1:7331".
	Relay string
	// RelayPublicKey pins the relay.
	RelayPublicKey []byte
	// Serve is the URI of the provider:
	// exec:///path/to/runtime?manifest=/path/to/capability.json
	Serve string
	// DirectPolicy is the file that names the peers that may carry a
	// conversation off the relay, and the addresses to use. Without it,
	// every conversation stays on the relay.
	DirectPolicy string
	// Log receives what the citizen drops and why.
	Log *slog.Logger
}

// Citizen is one serving citizen.
type Citizen struct {
	me       *identity.Identity
	pkg      map[string]any
	runtime  *host.Process
	relay    *client.Client
	log      *slog.Logger
	capID    string
	maxBytes int

	// binaryIn and binaryOut say that the capability carries bytes: the
	// request and the reply cross the runtime connection as base64.
	binaryIn  bool
	binaryOut bool

	direct *direct.Manager

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	carriers map[string]*direct.Conn
	sessions map[string]*session.Session
	guard    map[string]uint64
	waiting  map[string]*pending

	group sync.WaitGroup

	stopOnce sync.Once
	stopErr  error
}

// pending is one request that the provider still holds.
type pending struct {
	peer      []byte
	requestID []byte
}

// Serve starts the provider, joins the relay, and answers requests until the
// context ends.
func Serve(ctx context.Context, opts Options) (*Citizen, error) {
	if opts.Identity == nil {
		return nil, errors.New("citizen: an identity is needed")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}

	path, args, manifest, err := ParseServeURI(opts.Serve)
	if err != nil {
		return nil, err
	}

	pkg, err := capability.LoadFile(manifest)
	if err != nil {
		return nil, err
	}

	fields, _ := pkg["capability"].(map[string]any)
	capID, _ := fields["id"].(string)

	// A policy that does not hold is an error before the provider starts.
	var rules []direct.Rule
	if opts.DirectPolicy != "" {
		if rules, err = direct.LoadPolicy(opts.DirectPolicy); err != nil {
			return nil, err
		}
	}

	environment := []string{
		"ARC_IDENTITY=" + opts.Identity.Name(),
		"ARC_IDENTITY_SHORT=" + opts.Identity.ShortName(),
		"ARC_PUBLIC_KEY=" + opts.Identity.EncodePublicKey(),
	}

	provider, err := host.Start(path, args, environment, opts.Log)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	relay, err := client.Dial(ctx, opts.Relay, client.Options{
		Identity:       opts.Identity,
		RelayPublicKey: opts.RelayPublicKey,
	})
	if err != nil {
		cancel()
		provider.Stop()
		return nil, err
	}

	serving := &Citizen{
		me:        opts.Identity,
		pkg:       pkg,
		runtime:   provider,
		relay:     relay,
		log:       opts.Log,
		capID:     capID,
		maxBytes:  requestLimit(pkg),
		binaryIn:  bodyEncoding(pkg, "request_body") == "base64",
		binaryOut: bodyEncoding(pkg, "response_body") == "base64",
		ctx:       ctx,
		cancel:    cancel,
		carriers:  map[string]*direct.Conn{},
		sessions:  map[string]*session.Session{},
		guard:     map[string]uint64{},
		waiting:   map[string]*pending{},
	}

	if opts.DirectPolicy != "" {
		serving.direct = direct.NewManager(opts.Identity, rules, serving.sendControl, opts.Log)
	}

	if err := serving.announce(); err != nil {
		serving.Close()
		return nil, err
	}

	serving.group.Add(3)
	go serving.readPackets()
	go serving.readProvider()
	go serving.renew()

	return serving, nil
}

// PublicKey returns the key that callers address.
func (c *Citizen) PublicKey() []byte { return c.me.PublicKey }

// Done closes when the citizen stops.
func (c *Citizen) Done() <-chan struct{} { return c.ctx.Done() }

// Err says why the citizen stopped by itself: the relay connection ended,
// or the provider stopped. It is nil while the citizen serves, and after a
// stop that its owner asked for.
func (c *Citizen) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stopErr
}

// stop ends the citizen, and keeps the first reason.
func (c *Citizen) stop(cause error) {
	c.stopOnce.Do(func() {
		c.mu.Lock()
		c.stopErr = cause
		c.mu.Unlock()
		c.cancel()
	})
}

// Close stops the provider and leaves the relay.
func (c *Citizen) Close() error {
	c.cancel()
	if c.direct != nil {
		c.direct.Close()
	}
	c.relay.Close()
	err := c.runtime.Stop()
	c.group.Wait()
	return err
}

// ParseServeURI reads exec:///path/to/runtime?manifest=/path/to/file.json.
// No other form exists.
func ParseServeURI(raw string) (path string, args []string, manifest string, err error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "exec" {
		return "", nil, "", fmt.Errorf("citizen: the address must be exec:///path/to/runtime?manifest=/path/to/file")
	}

	path = parsed.Path
	if path == "" {
		return "", nil, "", fmt.Errorf("citizen: the address names no runtime")
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", nil, "", err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", nil, "", fmt.Errorf("citizen: %s is not a program", path)
	}

	query := parsed.Query()
	manifest = query.Get("manifest")
	if manifest == "" {
		return "", nil, "", fmt.Errorf("citizen: the address names no manifest")
	}
	if manifest, err = filepath.Abs(manifest); err != nil {
		return "", nil, "", err
	}

	if given := query.Get("args"); given != "" {
		args = strings.Fields(given)
	}
	return path, args, manifest, nil
}

// announce publishes the capability of this citizen to the relay.
func (c *Citizen) announce() error {
	summary, err := capability.Summary(c.me, c.pkg)
	if err != nil {
		return err
	}

	capabilities := make([]announce.Capability, 0, 1)
	for _, fields := range capability.SummaryCapabilities(summary) {
		capabilities = append(capabilities, announce.Capability{
			ID:             text(fields["id"]),
			Kind:           text(fields["kind"]),
			Scheme:         text(fields["scheme"]),
			Title:          text(fields["title"]),
			Summary:        text(fields["summary"]),
			InvocationMode: text(fields["invocation_mode"]),
			ReleaseVersion: text(fields["release_version"]),
			Channel:        text(fields["channel"]),
			DetailPath:     text(fields["detail_path"]),
		})
	}

	record, err := announce.Create(c.me, capabilities, announce.Options{})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	return c.relay.Announce(ctx, record)
}

// renew publishes the announcement again before it expires.
func (c *Citizen) renew() {
	defer c.group.Done()

	ticker := time.NewTicker(AnnounceEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := c.announce(); err != nil {
				c.log.Warn("the announcement did not renew", "error", err)
			}
		case <-c.ctx.Done():
			return
		}
	}
}

// readPackets answers every packet that reaches this citizen.
func (c *Citizen) readPackets() {
	defer c.group.Done()

	for {
		select {
		case raw, ok := <-c.relay.Packets():
			if !ok {
				c.relayLost()
				return
			}
			c.handlePacket(raw)
		case <-c.ctx.Done():
			return
		}
	}
}

// relayLost stops the citizen after its relay connection ends. A direct
// route that stands keeps its conversation until its lease ends. Without the
// relay it cannot renew, so the citizen stops when the last lease ends.
func (c *Citizen) relayLost() {
	cause := errors.New("the relay connection ended")
	if err := c.relay.Err(); err != nil {
		cause = fmt.Errorf("the relay connection ended: %w", err)
	}

	if c.direct != nil {
		if until := c.direct.ActiveUntil(); !until.IsZero() {
			c.log.Warn("the relay connection ended, and the direct routes serve until their leases end",
				"until", until.Format(time.RFC3339))

			timer := time.NewTimer(time.Until(until))
			defer timer.Stop()

			select {
			case <-timer.C:
			case <-c.ctx.Done():
				return
			}
		}
	}
	c.stop(cause)
}

func (c *Citizen) handlePacket(raw []byte) {
	decoded, err := packet.Decode(raw)
	if err != nil {
		c.log.Debug("a packet did not decode", "error", err)
		return
	}
	if string(decoded.Dst) != string(c.me.PublicKey) {
		return
	}
	if !c.fresh(decoded) {
		c.log.Debug("a packet is stale or replayed", "from", identity.Name(decoded.Src))
		return
	}

	talk, err := c.session(decoded)
	if err != nil {
		c.log.Debug("no session for a packet", "error", err)
		return
	}

	plaintext, err := talk.Decrypt(decoded.Nonce, decoded.Ciphertext)
	if err != nil {
		c.log.Debug("a packet did not decrypt", "from", identity.Name(decoded.Src))
		return
	}

	message, err := frame.Decode(plaintext)
	if err != nil {
		c.log.Debug("a frame did not decode", "error", err)
		return
	}

	// A peer that asks to carry this conversation off the relay says so in
	// an event of its own.
	if c.direct != nil && message.Type == frame.Event && message.Meta["topic"] == direct.Topic {
		c.direct.Control(decoded.Src, decoded.SessionID, message.Body)
		c.watchCarrier(decoded.Src)
		return
	}

	if message.Type != frame.Request {
		return
	}

	// A manifest request is answered by the citizen. Everything else goes to
	// the provider.
	if ask, id := capability.Request(message.Meta); ask != capability.AskNone {
		c.answerManifest(decoded.Src, message, ask, id)
		return
	}

	if len(message.Body) > c.maxBytes {
		c.fail(decoded.Src, message.RequestID, "request_too_large", "the body is over the limit")
		return
	}

	// A text provider reads JSON strings, which hold UTF-8 only. Other bytes
	// would change on the way, so they fail here.
	if !c.binaryIn && !utf8.Valid(message.Body) {
		c.fail(decoded.Src, message.RequestID, "invalid_body", "the body is not UTF-8, and the capability takes text")
		return
	}

	key := hex.EncodeToString(message.RequestID)
	if code, reason := c.admit(decoded.Src, message.RequestID); code != "" {
		c.fail(decoded.Src, message.RequestID, code, reason)
		return
	}

	event := map[string]any{
		"op":             "request",
		"message":        string(message.Body),
		"from":           hex.EncodeToString(decoded.Src),
		"meta":           message.Meta,
		"arc_session_id": hex.EncodeToString(decoded.SessionID),
		"app_session_id": nil,
		"request_id":     key,
		"framed":         true,
	}
	if c.binaryIn {
		event["encoding"] = "base64"
		event["message"] = base64.StdEncoding.EncodeToString(message.Body)
	}

	if err := c.runtime.Send(event); err != nil {
		c.mu.Lock()
		delete(c.waiting, key)
		c.mu.Unlock()
		c.fail(decoded.Src, message.RequestID, "provider_unavailable", err.Error())
	}
}

// admit takes one request into the set that waits for the provider, or says
// why not. A request id that already waits is refused, and so is a request
// over MaxPending. A slot frees when the provider answers or stops, and
// never when a caller gives up.
func (c *Citizen) admit(peer, requestID []byte) (code, reason string) {
	key := hex.EncodeToString(requestID)

	c.mu.Lock()
	defer c.mu.Unlock()

	switch {
	case c.waiting[key] != nil:
		return "duplicate_request", "a request with this id is waiting"
	case len(c.waiting) >= MaxPending:
		return "provider_busy", fmt.Sprintf("the provider holds %d requests", MaxPending)
	}
	c.waiting[key] = &pending{peer: peer, requestID: requestID}
	return "", ""
}

// readProvider carries the answers of the provider back to the callers.
func (c *Citizen) readProvider() {
	defer c.group.Done()

	for {
		select {
		case answer, ok := <-c.runtime.Lines():
			if !ok {
				c.log.Error("the provider stopped")
				c.stop(errors.New("the provider stopped"))
				return
			}
			c.handleAnswer(answer)
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Citizen) handleAnswer(answer map[string]any) {
	if event, _ := answer["op"].(string); event == "event" {
		c.sendEvent(answer)
		return
	}

	key, _ := answer["request_id"].(string)

	c.mu.Lock()
	held := c.waiting[key]
	delete(c.waiting, key)
	c.mu.Unlock()

	if held == nil {
		c.log.Debug("the provider answered a request that is not waiting", "request", key)
		return
	}

	if message, held2 := answer["error"]; held2 {
		c.fail(held.peer, held.requestID, "provider_error", text(message))
		return
	}

	reply, err := c.replyBody(answer)
	if err != nil {
		c.log.Warn("the provider wrote a reply that does not hold", "error", err)
		c.fail(held.peer, held.requestID, "invalid_reply", err.Error())
		return
	}

	body, err := frame.Encode(frame.Response, held.requestID, map[string]any{}, reply)
	if err != nil {
		c.log.Error("the answer did not encode", "error", err)
		return
	}
	c.send(held.peer, body)
}

// replyBody reads the reply of the provider. A capability that carries bytes
// gets them as base64, and a text capability gets them as they are.
func (c *Citizen) replyBody(answer map[string]any) ([]byte, error) {
	reply, _ := answer["reply"].(string)
	encoding, _ := answer["encoding"].(string)

	if !c.binaryOut {
		if encoding != "" {
			return nil, fmt.Errorf("the reply names the encoding %q, and the capability replies with text", encoding)
		}
		return []byte(reply), nil
	}

	if encoding != "base64" {
		return nil, errors.New("the capability replies with bytes, and the reply is not base64")
	}
	if base64.StdEncoding.DecodedLen(len(reply)) > MaxBodyBytes+2 {
		return nil, fmt.Errorf("the reply is over %d bytes", MaxBodyBytes)
	}
	body, err := base64.StdEncoding.DecodeString(reply)
	if err != nil {
		return nil, errors.New("the reply is not valid base64")
	}
	if len(body) > MaxBodyBytes {
		return nil, fmt.Errorf("the reply is over %d bytes", MaxBodyBytes)
	}
	return body, nil
}

// sendEvent carries an event of the provider to the citizen that it names.
func (c *Citizen) sendEvent(answer map[string]any) {
	to, err := hex.DecodeString(text(answer["to"]))
	if err != nil || len(to) != identity.SeedBytes {
		c.log.Warn("the provider named a citizen that is not a key")
		return
	}

	meta := map[string]any{}
	if fields, ok := answer["meta"].(map[string]any); ok {
		for key, value := range fields {
			meta[key] = value
		}
	}

	body, err := frame.EncodeEvent(text(answer["topic"]), []byte(text(answer["body"])), meta)
	if err != nil {
		c.log.Error("the event did not encode", "error", err)
		return
	}
	c.send(to, body)
}

func (c *Citizen) answerManifest(peer []byte, message *frame.Frame, ask capability.Ask, id string) {
	var document map[string]any
	var err error

	if ask == capability.AskSummary {
		document, err = capability.Summary(c.me, c.pkg)
	} else {
		document, err = capability.Detail(c.me, c.pkg, id)
	}
	if err != nil {
		c.fail(peer, message.RequestID, "not_found", "no capability of that name")
		return
	}

	body, err := json.Marshal(document)
	if err != nil {
		c.log.Error("the manifest did not encode", "error", err)
		return
	}

	reply, err := frame.Encode(frame.Response, message.RequestID, map[string]any{}, body)
	if err != nil {
		c.log.Error("the manifest frame did not encode", "error", err)
		return
	}
	c.send(peer, reply)
}

func (c *Citizen) fail(peer, requestID []byte, code, message string) {
	body, err := frame.EncodeError(requestID, code, message, nil, nil)
	if err != nil {
		c.log.Error("the error frame did not encode", "error", err)
		return
	}
	c.send(peer, body)
}

// sendControl carries one message of a direct route to a peer, over the
// relay, as an event of its own. It never takes the carrier: the peer reads
// the carrier only after the route stands, and the message that says so
// must reach it first.
func (c *Citizen) sendControl(peer, body []byte) error {
	event, err := frame.EncodeEvent(direct.Topic, body, nil)
	if err != nil {
		return err
	}

	raw, err := c.seal(peer, event)
	if err != nil {
		return err
	}
	return c.relay.SendPacket(raw)
}

// watchCarrier reads the packets of a carrier once a route stands, and
// answers them as if they had come over the relay.
func (c *Citizen) watchCarrier(peer []byte) {
	go func() {
		// The negotiation takes a moment. Look for the carrier until it
		// stands or the attempt ends.
		deadline := time.Now().Add(direct.AttemptLimit + time.Second)

		for time.Now().Before(deadline) {
			carrier := c.direct.CarrierFor(peer)
			if carrier == nil {
				time.Sleep(50 * time.Millisecond)
				continue
			}

			c.mu.Lock()
			if c.carriers[string(peer)] == carrier {
				c.mu.Unlock()
				return
			}
			c.carriers[string(peer)] = carrier
			c.mu.Unlock()

			for {
				select {
				case raw, ok := <-carrier.Packets():
					if !ok {
						c.mu.Lock()
						if c.carriers[string(peer)] == carrier {
							delete(c.carriers, string(peer))
						}
						c.mu.Unlock()
						return
					}
					c.handlePacket(raw)
				case <-c.ctx.Done():
					return
				}
			}
		}
	}()
}

// send encrypts one frame to a peer, and gives the packet to the relay, or
// to the carrier of that peer when one stands.
func (c *Citizen) send(peer, body []byte) {
	raw, err := c.seal(peer, body)
	if err != nil {
		c.log.Warn("the reply did not seal", "to", identity.Name(peer), "error", err)
		return
	}

	// A conversation that left the relay answers on its carrier.
	c.mu.Lock()
	carrier := c.carriers[string(peer)]
	c.mu.Unlock()

	if carrier != nil {
		if err := carrier.Send(raw); err == nil {
			return
		}
	}

	if err := c.relay.SendPacket(raw); err != nil {
		c.log.Warn("the reply did not reach the relay", "error", err)
	}
}

// seal encrypts one frame to a peer, and returns the packet.
func (c *Citizen) seal(peer, body []byte) ([]byte, error) {
	talk, err := c.sessionFor(peer)
	if err != nil {
		return nil, err
	}

	nonce, ciphertext, seq, err := talk.Encrypt(body)
	if err != nil {
		return nil, err
	}

	return packet.Encode(c.me, peer, talk.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(talk.EphemeralPublic))
}

// session returns the session of an arriving packet. A packet that names
// another session starts a new one, and replaces the one held.
func (c *Citizen) session(decoded *packet.Packet) (*session.Session, error) {
	if len(decoded.EphemeralPublic) != identity.SeedBytes {
		return nil, errors.New("citizen: the packet carries no ephemeral key")
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	key := string(decoded.Src)
	if held, ok := c.sessions[key]; ok && string(held.ID) == string(decoded.SessionID) {
		return held, nil
	}

	joined, err := session.Accept(c.me, decoded.Src, decoded.EphemeralPublic, decoded.SessionID)
	if err != nil {
		return nil, err
	}
	c.sessions[key] = joined
	return joined, nil
}

// sessionFor returns the session of a peer, and starts one when the citizen
// speaks first.
func (c *Citizen) sessionFor(peer []byte) (*session.Session, error) {
	c.mu.Lock()
	held, ok := c.sessions[string(peer)]
	c.mu.Unlock()

	if ok {
		return held, nil
	}

	started, err := session.Establish(c.me, peer)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.sessions[string(peer)] = started
	c.mu.Unlock()
	return started, nil
}

// fresh refuses a packet that is old, and a sequence number that already
// arrived on this session.
func (c *Citizen) fresh(decoded *packet.Packet) bool {
	skew := time.Since(time.UnixMilli(decoded.Timestamp))
	if skew < 0 {
		skew = -skew
	}
	if skew > MaxSkew {
		return false
	}

	key := string(decoded.Src) + string(decoded.SessionID)

	c.mu.Lock()
	defer c.mu.Unlock()

	held, ok := c.guard[key]
	if ok && decoded.Seq <= held {
		return false
	}
	c.guard[key] = decoded.Seq
	return true
}

// requestLimit reads the body limit of the capability, and holds it inside
// the limit of ARC.
func requestLimit(pkg map[string]any) int {
	fields, _ := pkg["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)
	body, _ := invocation["request_body"].(map[string]any)

	limit := MaxBodyBytes
	if given, ok := body["max_bytes"].(json.Number); ok {
		if value, err := given.Int64(); err == nil && value > 0 && int(value) < limit {
			limit = int(value)
		}
	}
	return limit
}

// bodyEncoding reads the encoding of the request or the reply body that the
// capability names: "base64" for bytes, and text otherwise.
func bodyEncoding(pkg map[string]any, which string) string {
	fields, _ := pkg["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)
	body, _ := invocation[which].(map[string]any)
	return text(body["encoding"])
}

func text(value any) string {
	out, _ := value.(string)
	return out
}
