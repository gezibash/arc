package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/gezibash/arc/announce"
	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/canonical"
	"github.com/gezibash/arc/internal/wire"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/session"
)

// Two relays that their operators approved for one another hold one
// authenticated link.
//
// The link starts with the ordinary relay handshake, which proves the
// Ed25519 key of each side. The two relays then exchange a short lived
// announcement and a proof that binds the X25519 key of the answering side
// to both nonces. After that, every message is an encrypted packet with a
// sequence number, so a frame of an older connection never counts on a new
// one.
//
// The frames of a link carry:
//
//	1 request    JSON, answered by a response with the same id
//	2 response   JSON
//	3 forward    one packet of a citizen, for a citizen of the other relay
//	4 ready      the first message of a link, which carries nothing
//	5 route      a routed packet, for a relay further on
const (
	proofDomain   = "ARC_FEDERATION_PROOF_V1"
	channelDomain = "ARC_FEDERATION_CHANNEL_V1"

	federationVersion = 1

	kindRequest  = 1
	kindResponse = 2
	kindForward  = 3
	kindReady    = 4
	kindRoute    = 5

	// MaxPeers is how many partners one relay approves.
	MaxPeers = 16
	// MaxFederationRequest caps one control message between relays.
	MaxFederationRequest = 256 * 1024
	// MaxForwardBytes caps one packet of a citizen on a link.
	MaxForwardBytes = 8 * 1024 * 1024

	federationRequestTimeout = 1250 * time.Millisecond
	federationHandshakeLimit = 1500 * time.Millisecond
	federationDialTimeout    = 700 * time.Millisecond
	federationReconnectPause = 250 * time.Millisecond
)

// Errors of the federation.
var (
	ErrNoPeer          = errors.New("relay: the peer is not connected")
	ErrPeerUnavailable = errors.New("relay: the peer did not take the message")
	ErrPeerTimeout     = errors.New("relay: the peer did not answer")
)

// Peer is one relay that this operator approved.
type Peer struct {
	// PublicKey is the identity of the partner.
	PublicKey []byte
	// Address is where to reach it, as host:port. A partner without an
	// address connects to this relay instead.
	Address string
}

// link is one connection to one partner.
type link struct {
	peer     []byte
	conn     net.Conn
	outbound bool

	// sending keeps the messages of this link in the order of their
	// sequence numbers. The partner refuses a message out of order.
	sending sync.Mutex

	mu          sync.Mutex
	ready       bool
	clientNonce []byte
	serverNonce []byte
	channel     []byte
	talk        *session.Session
	receivedSeq int64
}

// federation holds the links of one relay.
type federation struct {
	relay *Relay
	me    *identity.Identity
	peers map[string]Peer

	mu      sync.Mutex
	links   map[string]*link
	pending map[string]chan map[string]any
	closed  bool
}

func newFederation(r *Relay, peers []Peer) (*federation, error) {
	held := map[string]Peer{}

	for _, peer := range peers {
		if len(peer.PublicKey) != identity.SeedBytes {
			return nil, fmt.Errorf("relay: a peer key holds %d bytes", len(peer.PublicKey))
		}
		if string(peer.PublicKey) == string(r.identity.PublicKey) {
			return nil, errors.New("relay: a relay cannot be its own peer")
		}
		if _, twice := held[string(peer.PublicKey)]; twice {
			return nil, errors.New("relay: a peer stands twice")
		}
		held[string(peer.PublicKey)] = peer
	}

	if len(held) > MaxPeers {
		return nil, fmt.Errorf("relay: at most %d peers", MaxPeers)
	}

	return &federation{
		relay:   r,
		me:      r.identity,
		peers:   held,
		links:   map[string]*link{},
		pending: map[string]chan map[string]any{},
	}, nil
}

// start dials every partner whose key stands above ours. The relay with the
// lower key opens the connection, so two relays never hold two links.
func (f *federation) start() {
	for key, peer := range f.peers {
		if peer.Address == "" || string(f.me.PublicKey) >= key {
			continue
		}
		go f.keepDialing(peer)
	}
}

func (f *federation) stop() {
	f.mu.Lock()
	f.closed = true
	links := make([]*link, 0, len(f.links))
	for _, held := range f.links {
		links = append(links, held)
	}
	f.links = map[string]*link{}
	f.mu.Unlock()

	for _, held := range links {
		held.conn.Close()
	}
}

// approved names every partner that the operator approved, in key order.
func (f *federation) approved() [][]byte {
	out := make([][]byte, 0, len(f.peers))
	for key := range f.peers {
		out = append(out, []byte(key))
	}
	sort.Slice(out, func(left, right int) bool { return string(out[left]) < string(out[right]) })
	return out
}

// Connected names the partners that hold a ready link.
func (f *federation) connected() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()

	var peers [][]byte
	for key, held := range f.links {
		if held.isReady() {
			peers = append(peers, []byte(key))
		}
	}
	return peers
}

// -- the outbound side ------------------------------------------------------

// keepDialing holds one link to one partner, and opens it again when it ends.
func (f *federation) keepDialing(peer Peer) {
	for {
		f.mu.Lock()
		closed := f.closed
		f.mu.Unlock()

		if closed || f.relay.ctx.Err() != nil {
			return
		}

		if err := f.dial(peer); err != nil {
			f.relay.log.Debug("the peer link failed",
				"peer", identity.Name(peer.PublicKey), "error", err)
		}

		select {
		case <-time.After(federationReconnectPause):
		case <-f.relay.ctx.Done():
			return
		}
	}
}

// dial opens one link and serves it until it ends.
func (f *federation) dial(peer Peer) error {
	dialer := net.Dialer{Timeout: federationDialTimeout}
	socket, err := dialer.DialContext(f.relay.ctx, "tcp", peer.Address)
	if err != nil {
		return err
	}
	defer socket.Close()

	_ = socket.SetDeadline(time.Now().Add(federationHandshakeLimit))

	// The ordinary relay handshake proves the key of each side.
	relayKey, challenge, err := wire.ReadRelayHello(socket)
	if err != nil {
		return err
	}
	if string(relayKey) != string(peer.PublicKey) {
		return errors.New("relay: the partner answers with another key")
	}

	hello, err := wire.ClientHello(f.me, relayKey, challenge)
	if err != nil {
		return err
	}
	if _, err := socket.Write(hello); err != nil {
		return err
	}

	held := &link{peer: peer.PublicKey, conn: socket, outbound: true, receivedSeq: -1}
	held.clientNonce = make([]byte, 32)
	if _, err := rand.Read(held.clientNonce); err != nil {
		return err
	}

	record, err := announce.Create(f.me, nil, announce.Options{})
	if err != nil {
		return err
	}

	greeting, err := json.Marshal(map[string]any{
		"type": "hello", "nonce": hex.EncodeToString(held.clientNonce), "announcement": record,
	})
	if err != nil {
		return err
	}
	if err := wire.WriteFrame(socket, federationFrame(greeting)); err != nil {
		return err
	}

	if err := f.hold(held); err != nil {
		return err
	}
	defer f.drop(held)

	return f.read(held)
}

// hold puts a link in place. A partner holds one link at a time.
func (f *federation) hold(held *link) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		return errors.New("relay: the relay is stopping")
	}
	if existing, there := f.links[string(held.peer)]; there && existing.isReady() {
		return errors.New("relay: the partner holds a link already")
	}
	f.links[string(held.peer)] = held
	return nil
}

func (f *federation) drop(held *link) {
	f.mu.Lock()
	if f.links[string(held.peer)] == held {
		delete(f.links, string(held.peer))
	}
	f.mu.Unlock()

	held.conn.Close()
	f.relay.forgetPeer(held.peer)
}

// read serves one outbound link until it ends.
func (f *federation) read(held *link) error {
	for {
		payload, err := wire.ReadFrame(held.conn, f.relay.options.MaxFrameBytes)
		if err != nil {
			return err
		}

		body, ok := isFederationFrame(payload)
		if !ok {
			// The relay info frame of the partner, or a frame of another
			// kind. A link reads only federation frames.
			if _, isInfo := wire.DecodeRelayInfo(payload); isInfo {
				continue
			}
			continue
		}

		if err := f.handleFrame(held, body); err != nil {
			return err
		}
	}
}

// -- the inbound side -------------------------------------------------------

// inbound takes one federation frame from a connection that the relay
// accepted. It answers a hello, and passes anything else to the link.
func (f *federation) inbound(from *conn, payload []byte) {
	key := string(from.publicKey)

	f.mu.Lock()
	if _, approved := f.peers[key]; !approved {
		f.mu.Unlock()
		return
	}
	held := f.links[key]
	f.mu.Unlock()

	// A hello starts a link. Anything else belongs to the link that stands.
	// A signed record travels inside these frames, and a signature covers
	// the digits that arrived, so the numbers keep their form.
	if greeting, ok := decodeFields(payload); ok && greeting["type"] == "hello" {
		nonce, _ := greeting["nonce"].(string)
		record, _ := greeting["announcement"].(map[string]any)
		f.acceptHello(from, nonce, record)
		return
	}

	if held == nil || held.conn != from.socket {
		return
	}
	if err := f.handleFrame(held, payload); err != nil {
		f.drop(held)
	}
}

// ended drops the link of a partner whose connection to this relay ended.
// Without it the link stays ready, and the partner cannot open a new one.
func (f *federation) ended(from *conn) {
	f.mu.Lock()
	held := f.links[string(from.publicKey)]
	f.mu.Unlock()

	if held != nil && held.conn == from.socket {
		f.drop(held)
	}
}

// acceptHello answers the greeting of a partner with a proof that binds the
// X25519 key of this relay to both nonces.
func (f *federation) acceptHello(from *conn, nonceHex string, record map[string]any) {
	clientNonce, err := hex.DecodeString(nonceHex)
	if err != nil || len(clientNonce) != 32 {
		return
	}

	entry, err := announce.Verify(record, time.Now())
	if err != nil || string(entry.PublicKey) != string(from.publicKey) {
		return
	}

	f.mu.Lock()
	existing := f.links[string(from.publicKey)]
	f.mu.Unlock()

	if existing != nil && existing.isReady() {
		return
	}

	serverNonce := make([]byte, 32)
	if _, err := rand.Read(serverNonce); err != nil {
		return
	}

	mine, err := announce.Create(f.me, nil, announce.Options{})
	if err != nil {
		return
	}

	ownX, _, err := f.me.ToX25519()
	if err != nil {
		return
	}

	proof := f.me.Sign(proofMessage(from.publicKey, f.me.PublicKey, clientNonce, serverNonce, ownX))
	answer, err := json.Marshal(map[string]any{
		"type":         "proof",
		"nonce":        hex.EncodeToString(serverNonce),
		"announcement": mine,
		"proof":        hex.EncodeToString(proof),
	})
	if err != nil {
		return
	}

	held := &link{
		peer:        from.publicKey,
		conn:        from.socket,
		clientNonce: clientNonce,
		serverNonce: serverNonce,
		channel:     channelOf(clientNonce, serverNonce),
		receivedSeq: -1,
	}

	if err := f.hold(held); err != nil {
		return
	}
	if !from.send(federationFrame(answer)) {
		f.drop(held)
	}
}

// -- the frames of a link ---------------------------------------------------

// handleFrame reads one frame of a link: the proof of a partner, or an
// encrypted message.
func (f *federation) handleFrame(held *link, payload []byte) error {
	held.mu.Lock()
	ready := held.ready
	outbound := held.outbound
	held.mu.Unlock()

	if !ready && outbound {
		return f.acceptProof(held, payload)
	}
	return f.decrypt(held, payload)
}

// acceptProof reads the answer of the partner, checks the proof, and starts
// the encrypted channel.
func (f *federation) acceptProof(held *link, payload []byte) error {
	answer, ok := decodeFields(payload)
	if !ok || answer["type"] != "proof" {
		return errors.New("relay: the partner did not prove itself")
	}

	nonceHex, _ := answer["nonce"].(string)
	proofHex, _ := answer["proof"].(string)
	record, _ := answer["announcement"].(map[string]any)

	serverNonce, err := hex.DecodeString(nonceHex)
	if err != nil || len(serverNonce) != 32 {
		return ErrPeerUnavailable
	}

	proof, err := hex.DecodeString(proofHex)
	if err != nil || len(proof) != 64 {
		return ErrPeerUnavailable
	}

	entry, err := announce.Verify(record, time.Now())
	if err != nil || string(entry.PublicKey) != string(held.peer) {
		return ErrPeerUnavailable
	}

	peerX, err := identity.PublicKeyToX25519(held.peer)
	if err != nil {
		return ErrPeerUnavailable
	}

	message := proofMessage(f.me.PublicKey, held.peer, held.clientNonce, serverNonce, peerX)
	if !identity.Verify(held.peer, message, proof) {
		return ErrPeerUnavailable
	}

	talk, err := session.Establish(f.me, held.peer)
	if err != nil {
		return err
	}

	held.mu.Lock()
	held.serverNonce = serverNonce
	held.channel = channelOf(held.clientNonce, serverNonce)
	held.talk = talk
	held.ready = true
	held.mu.Unlock()

	_ = held.conn.SetDeadline(time.Time{})

	// The first encrypted message tells the partner that the link stands.
	if err := f.send(held, kindReady, "", nil); err != nil {
		return err
	}

	f.relay.peerUp(held.peer)
	return nil
}

// decrypt reads one encrypted message of a link.
func (f *federation) decrypt(held *link, payload []byte) error {
	decoded, err := packet.Decode(payload)
	if err != nil {
		return ErrPeerUnavailable
	}
	if string(decoded.Src) != string(held.peer) || string(decoded.Dst) != string(f.me.PublicKey) {
		return ErrPeerUnavailable
	}

	held.mu.Lock()
	first := held.talk == nil
	talk := held.talk
	channel := held.channel
	expected := held.receivedSeq + 1
	held.mu.Unlock()

	if first {
		// The partner speaks first on an inbound link. Its packet names the
		// session, and the sequence starts at zero.
		if decoded.Seq != 0 || len(decoded.EphemeralPublic) != identity.SeedBytes {
			return ErrPeerUnavailable
		}

		joined, err := session.Accept(f.me, decoded.Src, decoded.EphemeralPublic, decoded.SessionID)
		if err != nil {
			return ErrPeerUnavailable
		}
		talk = joined
	} else {
		if string(decoded.SessionID) != string(talk.ID) || int64(decoded.Seq) != expected {
			return ErrPeerUnavailable
		}
	}

	plaintext, err := talk.Decrypt(decoded.Nonce, decoded.Ciphertext)
	if err != nil {
		return ErrPeerUnavailable
	}

	kind, requestID, body, err := decodePlaintext(channel, plaintext)
	if err != nil {
		return err
	}

	held.mu.Lock()
	held.talk = talk
	held.receivedSeq = int64(decoded.Seq)
	if first {
		held.ready = true
	}
	held.mu.Unlock()

	if first {
		_ = held.conn.SetDeadline(time.Time{})
		f.relay.peerUp(held.peer)
	}

	f.dispatch(held, kind, requestID, body)
	return nil
}

// dispatch hands one message to the part of the relay that answers it.
func (f *federation) dispatch(held *link, kind int, requestID string, body []byte) {
	switch kind {
	case kindReady:
		return

	case kindRequest:
		request, ok := decodeFields(body)
		if !ok {
			return
		}

		// A lookup asks further partners and takes seconds, so it runs apart
		// from the link, which keeps reading.
		if kind, _ := request["type"].(string); kind == "search" || kind == "resolve" {
			go func() {
				answer := f.relay.answerLookup(held.peer, request)
				if err := f.send(held, kindResponse, requestID, answer); err != nil {
					f.relay.log.Debug("the answer did not reach the peer", "error", err)
				}
			}()
			return
		}

		answer := f.relay.answerPeer(held.peer, request)
		if answer == nil {
			return
		}
		if err := f.send(held, kindResponse, requestID, answer); err != nil {
			f.relay.log.Debug("the answer did not reach the peer", "error", err)
		}

	case kindResponse:
		answer, ok := decodeFields(body)
		if !ok {
			return
		}

		f.mu.Lock()
		waiting := f.pending[requestID]
		delete(f.pending, requestID)
		f.mu.Unlock()

		if waiting != nil {
			waiting <- answer
		}

	case kindForward:
		f.relay.carryFromPeer(held.peer, body)

	case kindRoute:
		f.relay.carryRoute(held.peer, body)
	}
}

// -- sending ----------------------------------------------------------------

// request asks a partner and waits for its answer.
func (f *federation) request(peer []byte, body map[string]any, timeout time.Duration) (map[string]any, error) {
	held, err := f.readyLink(peer)
	if err != nil {
		return nil, err
	}

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return nil, err
	}
	requestID := string(id)

	answers := make(chan map[string]any, 1)

	f.mu.Lock()
	f.pending[requestID] = answers
	f.mu.Unlock()

	defer func() {
		f.mu.Lock()
		delete(f.pending, requestID)
		f.mu.Unlock()
	}()

	if err := f.send(held, kindRequest, requestID, body); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case answer := <-answers:
		return answer, nil
	case <-timer.C:
		return nil, ErrPeerTimeout
	case <-f.relay.ctx.Done():
		return nil, ErrPeerUnavailable
	}
}

// forward passes one packet of a citizen to the relay of its destination.
func (f *federation) forward(peer, raw []byte) error {
	held, err := f.readyLink(peer)
	if err != nil {
		return err
	}
	return f.sendBytes(held, kindForward, raw)
}

// forwardRoute passes one routed packet to the next relay of its path.
func (f *federation) forwardRoute(peer, raw []byte) error {
	held, err := f.readyLink(peer)
	if err != nil {
		return err
	}
	return f.sendBytes(held, kindRoute, raw)
}

func (f *federation) readyLink(peer []byte) (*link, error) {
	f.mu.Lock()
	held := f.links[string(peer)]
	f.mu.Unlock()

	if held == nil || !held.isReady() {
		return nil, ErrNoPeer
	}
	return held, nil
}

// send writes one message with a JSON body, or none.
func (f *federation) send(held *link, kind int, requestID string, body map[string]any) error {
	var encoded []byte

	if body != nil {
		marshalled, err := json.Marshal(body)
		if err != nil {
			return err
		}
		if len(marshalled) > MaxFederationRequest {
			return errors.New("relay: the message is too large")
		}
		encoded = marshalled
	}

	return f.write(held, kind, requestID, encoded)
}

func (f *federation) sendBytes(held *link, kind int, body []byte) error {
	if len(body) > MaxForwardBytes+MaxRouteRecord+MaxRoutePath*identity.SeedBytes+10 {
		return errors.New("relay: the message is too large")
	}
	return f.write(held, kind, "", body)
}

// write encrypts one message and gives it to the connection.
func (f *federation) write(held *link, kind int, requestID string, body []byte) error {
	held.mu.Lock()
	talk := held.talk
	channel := held.channel
	held.mu.Unlock()

	if talk == nil {
		return ErrNoPeer
	}

	plaintext, err := encodePlaintext(channel, kind, requestID, body)
	if err != nil {
		return err
	}

	held.sending.Lock()
	defer held.sending.Unlock()

	nonce, ciphertext, seq, err := talk.Encrypt(plaintext)
	if err != nil {
		return err
	}

	raw, err := packet.Encode(f.me, held.peer, talk.ID, seq, nonce, ciphertext,
		packet.WithEphemeralKey(talk.EphemeralPublic))
	if err != nil {
		return err
	}

	if err := wire.WriteFrame(held.conn, federationFrame(raw)); err != nil {
		return ErrPeerUnavailable
	}
	return nil
}

// -- the shape of one message -----------------------------------------------

// encodePlaintext writes the body of one message:
//
//	version, kind, channel[32], request id[16] for a request or a response,
//	then the body.
func encodePlaintext(channel []byte, kind int, requestID string, body []byte) ([]byte, error) {
	if len(channel) != sha256.Size {
		return nil, ErrPeerUnavailable
	}

	out := make([]byte, 0, 2+len(channel)+16+len(body))
	out = append(out, federationVersion, uint8(kind))
	out = append(out, channel...)

	switch kind {
	case kindRequest, kindResponse:
		if len(requestID) != 16 {
			return nil, errors.New("relay: a request needs an id of 16 bytes")
		}
		out = append(out, requestID...)
	case kindForward, kindRoute, kindReady:
		if requestID != "" {
			return nil, errors.New("relay: this message carries no id")
		}
	default:
		return nil, errors.New("relay: unknown message")
	}

	return append(out, body...), nil
}

// decodePlaintext reads one message, and refuses one that names another
// channel.
func decodePlaintext(channel, plaintext []byte) (kind int, requestID string, body []byte, err error) {
	if len(plaintext) < 2+sha256.Size || plaintext[0] != federationVersion {
		return 0, "", nil, ErrPeerUnavailable
	}

	kind = int(plaintext[1])
	if string(plaintext[2:2+sha256.Size]) != string(channel) {
		return 0, "", nil, ErrPeerUnavailable
	}

	rest := plaintext[2+sha256.Size:]

	switch kind {
	case kindRequest, kindResponse:
		if len(rest) < 16 || len(rest)-16 > MaxFederationRequest {
			return 0, "", nil, ErrPeerUnavailable
		}
		return kind, string(rest[:16]), rest[16:], nil

	case kindForward:
		if len(rest) > MaxForwardBytes {
			return 0, "", nil, ErrPeerUnavailable
		}
		return kind, "", rest, nil

	case kindRoute:
		return kind, "", rest, nil

	case kindReady:
		if len(rest) != 0 {
			return 0, "", nil, ErrPeerUnavailable
		}
		return kind, "", nil, nil

	default:
		return 0, "", nil, ErrPeerUnavailable
	}
}

func (l *link) isReady() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.ready
}

// channelOf binds a link to its two nonces, so a frame of an older
// connection never counts on a new one.
func channelOf(clientNonce, serverNonce []byte) []byte {
	digest := sha256.Sum256(append(append([]byte(channelDomain), clientNonce...), serverNonce...))
	return digest[:]
}

// proofMessage is what the answering relay signs.
func proofMessage(clientKey, serverKey, clientNonce, serverNonce, serverX []byte) []byte {
	out := make([]byte, 0, len(proofDomain)+len(clientKey)+len(serverKey)+64+32)
	out = append(out, proofDomain...)
	out = append(out, clientKey...)
	out = append(out, serverKey...)
	out = append(out, clientNonce...)
	out = append(out, serverNonce...)
	return append(out, serverX...)
}

// decodeFields reads the JSON of a link. The numbers keep their digits,
// because a signed record travels inside and its signature covers the bytes
// that arrived.
func decodeFields(payload []byte) (map[string]any, bool) {
	value, err := canonical.Decode(payload)
	if err != nil {
		return nil, false
	}

	fields, ok := value.(map[string]any)
	return fields, ok
}

func federationFrame(body []byte) []byte {
	return append([]byte(wire.FederationPrefix), body...)
}

func isFederationFrame(payload []byte) ([]byte, bool) {
	if len(payload) < len(wire.FederationPrefix) ||
		string(payload[:len(wire.FederationPrefix)]) != wire.FederationPrefix {
		return nil, false
	}
	return payload[len(wire.FederationPrefix):], true
}
