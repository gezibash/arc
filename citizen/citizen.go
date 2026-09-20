package citizen

import (
	"context"
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

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/capability"
	"github.com/gezibash/arc/client"
	"github.com/gezibash/arc/direct"
	"github.com/gezibash/arc/frame"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/session"
)

// The timings of a serving citizen.
const (
	// AnnounceEvery is how often the citizen renews its announcement. The
	// record lives for 180 seconds.
	AnnounceEvery = 150 * time.Second
	// MaxSkew is how far the time of a packet may stand from the time here.
	MaxSkew = 120 * time.Second
	// MaxBodyBytes caps one request body.
	MaxBodyBytes = 1024 * 1024
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
	runtime  *runtime
	relay    *client.Client
	log      *slog.Logger
	capID    string
	maxBytes int

	direct *direct.Manager

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	carriers map[string]*direct.Conn
	sessions map[string]*session.Session
	guard    map[string]uint64
	waiting  map[string]*pending

	group sync.WaitGroup
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

	environment := []string{
		"ARC_IDENTITY=" + opts.Identity.Name(),
		"ARC_IDENTITY_SHORT=" + opts.Identity.ShortName(),
		"ARC_PUBLIC_KEY=" + opts.Identity.EncodePublicKey(),
	}

	provider, err := startRuntime(path, args, environment, opts.Log)
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
		provider.stop()
		return nil, err
	}

	serving := &Citizen{
		me:       opts.Identity,
		pkg:      pkg,
		runtime:  provider,
		relay:    relay,
		log:      opts.Log,
		capID:    capID,
		maxBytes: requestLimit(pkg),
		ctx:      ctx,
		cancel:   cancel,
		carriers: map[string]*direct.Conn{},
		sessions: map[string]*session.Session{},
		guard:    map[string]uint64{},
		waiting:  map[string]*pending{},
	}

	if opts.DirectPolicy != "" {
		rules, err := direct.LoadPolicy(opts.DirectPolicy)
		if err != nil {
			serving.Close()
			return nil, err
		}
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

// Close stops the provider and leaves the relay.
func (c *Citizen) Close() error {
	c.cancel()
	if c.direct != nil {
		c.direct.Close()
	}
	c.relay.Close()
	err := c.runtime.stop()
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
				c.cancel()
				return
			}
			c.handlePacket(raw)
		case <-c.ctx.Done():
			return
		}
	}
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

	key := hex.EncodeToString(message.RequestID)

	c.mu.Lock()
	c.waiting[key] = &pending{peer: decoded.Src, requestID: message.RequestID}
	c.mu.Unlock()

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

	if err := c.runtime.send(event); err != nil {
		c.mu.Lock()
		delete(c.waiting, key)
		c.mu.Unlock()
		c.fail(decoded.Src, message.RequestID, "provider_unavailable", err.Error())
	}
}

// readProvider carries the answers of the provider back to the callers.
func (c *Citizen) readProvider() {
	defer c.group.Done()

	for {
		select {
		case answer, ok := <-c.runtime.lines:
			if !ok {
				c.log.Error("the provider stopped")
				c.cancel()
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

	reply, _ := answer["reply"].(string)
	body, err := frame.Encode(frame.Response, held.requestID, map[string]any{}, []byte(reply))
	if err != nil {
		c.log.Error("the answer did not encode", "error", err)
		return
	}
	c.send(held.peer, body)
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
// relay, as an event of its own.
func (c *Citizen) sendControl(peer, body []byte) error {
	event, err := frame.EncodeEvent(direct.Topic, body, nil)
	if err != nil {
		return err
	}

	c.send(peer, event)
	return nil
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
	talk, err := c.sessionFor(peer)
	if err != nil {
		c.log.Warn("no session for a reply", "to", identity.Name(peer), "error", err)
		return
	}

	nonce, ciphertext, seq, err := talk.Encrypt(body)
	if err != nil {
		c.log.Error("the reply did not encrypt", "error", err)
		return
	}

	raw, err := packet.Encode(c.me, peer, talk.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		c.log.Error("the reply did not encode", "error", err)
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

func text(value any) string {
	out, _ := value.(string)
	return out
}
