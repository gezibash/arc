// Package relay holds the relay server.
//
// A relay is a blind forwarder. It reads the header of a packet, checks the
// signature of the sender, and passes the packet to the connection of the
// destination. It never holds a key of a citizen, and never reads a body.
//
// A relay also keeps a small directory. A citizen announces the capabilities
// that it offers, and another citizen resolves a petname or searches for a
// capability. Every record is signed, short lived, and public.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/wire"
	"github.com/gezibash/arc/packet"
)

// The limits of a relay.
const (
	// MaxSkew is how far the time of a packet may stand from the time of the
	// relay.
	MaxSkew = 120 * time.Second
	// SendQueue is the number of packets that wait for one slow connection.
	SendQueue = 2048
	// HelloTimeout bounds the handshake of a new connection.
	HelloTimeout = 5 * time.Second
)

// Options holds what a relay needs to run.
type Options struct {
	// Identity is the identity of the relay. It is needed.
	Identity *identity.Identity
	// Address is where the relay listens, for example ":7331".
	Address string
	// MaxFrameBytes caps one frame. Zero accepts a frame of any size, and the
	// relay tells each client what it accepts.
	MaxFrameBytes uint32
	// Version is what the relay reports in its status.
	Version string
	// Log receives what the relay drops and why. The default is the standard
	// logger.
	Log *slog.Logger
	// Peers are the relays that this operator approved. A relay without
	// peers serves its own citizens only.
	Peers []Peer
	// Transit lets traffic of other relays pass through this one. It is off
	// by default, and it never widens what a publisher signed.
	Transit bool
}

// Relay is one running relay.
type Relay struct {
	identity *identity.Identity
	options  Options
	listener net.Listener
	log      *slog.Logger
	started  time.Time

	ctx    context.Context
	cancel context.CancelFunc

	federation *federation
	catalog    *catalog

	mu        sync.RWMutex
	routes    map[string]*conn
	directory map[string]*record
	paths     map[string]*conversation

	group sync.WaitGroup
}

// Listen starts a relay. The relay serves until Close, and Addr says where it
// listens.
func Listen(ctx context.Context, opts Options) (*Relay, error) {
	if opts.Identity == nil {
		return nil, errors.New("relay: an identity is needed")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Version == "" {
		opts.Version = "unknown"
	}

	listener, err := net.Listen("tcp", opts.Address)
	if err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(ctx)
	relay := &Relay{
		identity:  opts.Identity,
		options:   opts,
		listener:  listener,
		log:       opts.Log,
		started:   time.Now(),
		ctx:       ctx,
		cancel:    cancel,
		routes:    map[string]*conn{},
		directory: map[string]*record{},
		paths:     map[string]*conversation{},
	}

	held, err := newFederation(relay, opts.Peers)
	if err != nil {
		cancel()
		listener.Close()
		return nil, err
	}
	relay.federation = held
	relay.catalog = newCatalog(relay)

	relay.group.Add(1)
	go relay.accept()
	held.start()

	return relay, nil
}

// Addr says where the relay listens.
func (r *Relay) Addr() net.Addr { return r.listener.Addr() }

// PublicKey returns the public key that the relay proves to each client.
func (r *Relay) PublicKey() []byte { return r.identity.PublicKey }

// Close stops the relay and every connection.
func (r *Relay) Close() error {
	r.cancel()
	r.federation.stop()
	err := r.listener.Close()

	r.mu.Lock()
	connections := make([]*conn, 0, len(r.routes))
	for _, connection := range r.routes {
		connections = append(connections, connection)
	}
	r.mu.Unlock()

	for _, connection := range connections {
		connection.close()
	}

	r.group.Wait()
	return err
}

// Connections says how many citizens hold a route right now.
func (r *Relay) Connections() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.routes)
}

func (r *Relay) accept() {
	defer r.group.Done()

	for {
		socket, err := r.listener.Accept()
		if err != nil {
			if r.ctx.Err() == nil {
				r.log.Error("the relay stopped accepting", "error", err)
			}
			return
		}

		r.group.Add(1)
		go func() {
			defer r.group.Done()
			r.serve(socket)
		}()
	}
}

// serve runs one connection: the handshake, then the frames.
func (r *Relay) serve(socket net.Conn) {
	defer socket.Close()

	publicKey, err := r.handshake(socket)
	if err != nil {
		r.log.Debug("the handshake failed", "peer", socket.RemoteAddr().String(), "error", err)
		return
	}

	connection := newConn(socket, publicKey, r.log)
	r.register(connection)
	defer r.forget(connection)

	go connection.write()
	defer connection.close()

	for {
		payload, err := wire.ReadFrame(socket, r.options.MaxFrameBytes)
		if err != nil {
			if !errors.Is(err, io.EOF) && r.ctx.Err() == nil {
				r.log.Debug("the connection ended", "citizen", identity.Name(publicKey), "error", err)
			}
			return
		}

		if control, ok := wire.IsDirectory(payload); ok {
			r.directoryControl(connection, control)
			continue
		}
		if control, ok := isFederationFrame(payload); ok {
			r.federation.inbound(connection, control)
			continue
		}
		r.route(connection, payload)
	}
}

// handshake proves the relay to the client, and the client to the relay.
func (r *Relay) handshake(socket net.Conn) ([]byte, error) {
	_ = socket.SetDeadline(time.Now().Add(HelloTimeout))
	defer socket.SetDeadline(time.Time{})

	challenge, err := newChallenge()
	if err != nil {
		return nil, err
	}

	hello, err := wire.RelayHello(r.identity.PublicKey, challenge)
	if err != nil {
		return nil, err
	}
	if _, err := socket.Write(hello); err != nil {
		return nil, err
	}
	if err := wire.WriteFrame(socket, wire.RelayInfo(r.options.MaxFrameBytes)); err != nil {
		return nil, err
	}

	answer := make([]byte, wire.ClientHelloBytes)
	if _, err := io.ReadFull(socket, answer); err != nil {
		return nil, err
	}

	publicKey, ok := wire.VerifyClientHello(answer, r.identity.PublicKey, challenge)
	if !ok {
		return nil, errors.New("relay: the client hello did not verify")
	}
	return publicKey, nil
}

// route forwards one packet. The relay drops a packet that it cannot place,
// and never answers the sender, because a relay is not a party to the message.
func (r *Relay) route(from *conn, raw []byte) {
	decoded, err := packet.Decode(raw)
	if err != nil {
		r.drop(raw, "the packet is not valid")
		return
	}
	if !fresh(decoded) {
		r.drop(raw, "the packet is stale")
		return
	}

	// The sender may write only under its own key.
	if string(decoded.Src) != string(from.publicKey) {
		r.drop(raw, "the sender is not the owner of the key")
		return
	}

	r.mu.RLock()
	destination := r.routes[string(decoded.Dst)]
	r.mu.RUnlock()

	if destination == nil {
		// A citizen of another relay may still be reachable over a partner.
		if !r.carryAway(decoded, raw) {
			r.drop(raw, "no route")
		}
		return
	}
	if !destination.send(raw) {
		r.drop(raw, "the destination is behind")
	}
}

func (r *Relay) register(connection *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// One citizen holds one route. A new connection takes the route, and the
	// old connection stops receiving.
	r.routes[string(connection.publicKey)] = connection
}

func (r *Relay) forget(connection *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := string(connection.publicKey)
	if r.routes[key] == connection {
		delete(r.routes, key)
	}
	if held, ok := r.directory[key]; ok && held.conn == connection {
		delete(r.directory, key)
	}
}

func (r *Relay) drop(raw []byte, reason string) {
	r.log.Debug("the relay dropped a packet", "bytes", len(raw), "reason", reason)
}

// fresh follows the Elixir relay: the time of a packet stands inside the
// skew, and the keys and the session id have the right length.
func fresh(decoded *packet.Packet) bool {
	skew := time.Since(time.UnixMilli(decoded.Timestamp))
	if skew < 0 {
		skew = -skew
	}
	return skew <= MaxSkew &&
		len(decoded.Src) == identity.SeedBytes &&
		len(decoded.Dst) == identity.SeedBytes &&
		len(decoded.SessionID) == 16
}

func newChallenge() ([]byte, error) {
	challenge := make([]byte, wire.ChallengeBytes)
	if _, err := randRead(challenge); err != nil {
		return nil, fmt.Errorf("relay: the system gave no random bytes: %w", err)
	}
	return challenge, nil
}
