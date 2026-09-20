package direct

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/wire"
)

// The limits of a carrier.
const (
	// MaxPacketBytes is the largest packet that a carrier takes.
	MaxPacketBytes = 1_114_112
	// DefaultTimeout bounds the handshake and one write.
	DefaultTimeout = 5 * time.Second
	// ExporterLabel names the keys that the proof covers.
	ExporterLabel = "EXPORTER-ARC-DIRECT-V1"
	// HelloDomain and FinishDomain stand at the front of each proof.
	HelloDomain  = "arc-direct-hello-v1"
	FinishDomain = "arc-direct-finish-v1"

	helloKind  = 1
	finishKind = 2
)

// Errors of the carrier.
var (
	ErrNoCertificate       = errors.New("direct: the peer sent no certificate")
	ErrCertificateMismatch = errors.New("direct: the certificate is not the one the relay named")
	ErrPeerKeyMismatch     = errors.New("direct: the peer proved another identity")
	ErrInvalidProof        = errors.New("direct: the proof of the peer does not hold")
	ErrMisdirected         = errors.New("direct: the proof names another connection")
	ErrPacketTooLarge      = errors.New("direct: the packet is too large")
	ErrClosed              = errors.New("direct: the carrier is closed")
	ErrInvalidOptions      = errors.New("direct: the options do not hold together")
)

// Options are what both sides agreed over the relay.
type Options struct {
	// Identity is the local identity.
	Identity *identity.Identity
	// Credentials are the certificate of this side.
	Credentials *Credentials
	// PeerKey is the identity that the other side must prove.
	PeerKey []byte
	// PeerFingerprint is the certificate that the other side must present.
	PeerFingerprint []byte
	// Binding ties this carrier to one conversation of the relay.
	Binding []byte
	// Timeout bounds the handshake and one write.
	Timeout time.Duration

	// direction names the side that listens on this carrier. The manager
	// sets it, and the carrier itself does not read it.
	direction string
}

func (o *Options) check() error {
	if o.Identity == nil || o.Credentials == nil {
		return ErrInvalidOptions
	}
	if len(o.PeerKey) != identity.SeedBytes || len(o.PeerFingerprint) != 32 || len(o.Binding) != 32 {
		return ErrInvalidOptions
	}
	return nil
}

func (o *Options) timeout() time.Duration {
	if o.Timeout <= 0 {
		return DefaultTimeout
	}
	return o.Timeout
}

// Conn is one carrier between two citizens.
type Conn struct {
	socket  *tls.Conn
	options Options

	packets chan []byte
	done    chan struct{}

	mu     sync.Mutex
	closed bool
	reason error
}

// Listener waits for the other side to arrive.
type Listener struct {
	inner   net.Listener
	options Options
}

// Listen waits for one carrier on an address. An address of "127.0.0.1:0"
// takes any free port, which Addr then names.
func Listen(address string, opts Options) (*Listener, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}

	inner, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	return &Listener{inner: inner, options: opts}, nil
}

// Addr says where the listener waits.
func (l *Listener) Addr() net.Addr { return l.inner.Addr() }

// Close stops the listener.
func (l *Listener) Close() error { return l.inner.Close() }

// Accept takes the next connection and proves both sides.
func (l *Listener) Accept(ctx context.Context) (*Conn, error) {
	socket, err := l.inner.Accept()
	if err != nil {
		return nil, err
	}

	held := tls.Server(socket, l.options.tlsConfig(true))
	return finish(ctx, held, l.options)
}

// Dial opens one carrier to an address, and proves both sides.
func Dial(ctx context.Context, address string, opts Options) (*Conn, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}

	dialer := net.Dialer{Timeout: opts.timeout()}
	socket, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	held := tls.Client(socket, opts.tlsConfig(false))
	return finish(ctx, held, opts)
}

// Accept proves both sides on a connection that the caller already holds.
// The simultaneous open of a hole punch gives one.
func Accept(ctx context.Context, socket net.Conn, opts Options, server bool) (*Conn, error) {
	if err := opts.check(); err != nil {
		return nil, err
	}

	var held *tls.Conn
	if server {
		held = tls.Server(socket, opts.tlsConfig(true))
	} else {
		held = tls.Client(socket, opts.tlsConfig(false))
	}
	return finish(ctx, held, opts)
}

// finish runs the TLS handshake and the proof of both identities.
func finish(ctx context.Context, socket *tls.Conn, opts Options) (*Conn, error) {
	deadline := time.Now().Add(opts.timeout())
	if held, ok := ctx.Deadline(); ok && held.Before(deadline) {
		deadline = held
	}
	_ = socket.SetDeadline(deadline)

	if err := socket.HandshakeContext(ctx); err != nil {
		socket.Close()
		return nil, err
	}

	if err := prove(socket, opts); err != nil {
		socket.Close()
		return nil, err
	}

	_ = socket.SetDeadline(time.Time{})

	held := &Conn{
		socket:  socket,
		options: opts,
		packets: make(chan []byte, 16),
		done:    make(chan struct{}),
	}

	go held.read()
	return held, nil
}

// Packets returns the packets that arrive. The channel closes with the
// carrier.
func (c *Conn) Packets() <-chan []byte { return c.packets }

// Done closes when the carrier ends, and Err says why.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns the reason the carrier ended.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// PeerKey is the identity that the other side proved.
func (c *Conn) PeerKey() []byte { return c.options.PeerKey }

// Send writes one packet.
func (c *Conn) Send(raw []byte) error {
	if len(raw) > MaxPacketBytes {
		return ErrPacketTooLarge
	}

	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()

	if closed {
		return ErrClosed
	}

	_ = c.socket.SetWriteDeadline(time.Now().Add(c.options.timeout()))
	if err := wire.WriteFrame(c.socket, raw); err != nil {
		c.stop(err)
		return err
	}
	return nil
}

// Close ends the carrier.
func (c *Conn) Close() error {
	c.stop(ErrClosed)
	return nil
}

func (c *Conn) read() {
	defer close(c.packets)

	for {
		raw, err := wire.ReadFrame(c.socket, MaxPacketBytes)
		if err != nil {
			c.stop(err)
			return
		}

		select {
		case c.packets <- raw:
		case <-c.done:
			return
		}
	}
}

func (c *Conn) stop(reason error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.reason = reason
	c.mu.Unlock()

	close(c.done)
	c.socket.Close()
}

// prove exchanges the identity proof of both sides. Each proof covers the
// keys of this TLS channel and the conversation of the relay, so it counts
// on this connection alone.
func prove(socket *tls.Conn, opts Options) error {
	state := socket.ConnectionState()
	exporter, err := state.ExportKeyingMaterial(ExporterLabel, opts.Binding, 32)
	if err != nil {
		return err
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}

	// hello: the identity of this side, a fresh nonce, and its certificate.
	body := make([]byte, 0, 1+32*3)
	body = append(body, helloKind)
	body = append(body, opts.Identity.PublicKey...)
	body = append(body, nonce...)
	body = append(body, opts.Credentials.Fingerprint...)

	hello := append(body, opts.Identity.Sign(join(HelloDomain, exporter, opts.Binding, body))...)
	if _, err := socket.Write(hello); err != nil {
		return err
	}

	peerNonce, err := readHello(socket, opts, exporter)
	if err != nil {
		return err
	}

	// finish: both identities, both nonces, and both certificates, so
	// neither side can pass one proof to a third party.
	body = make([]byte, 0, 1+32*6)
	body = append(body, finishKind)
	body = append(body, opts.Identity.PublicKey...)
	body = append(body, nonce...)
	body = append(body, opts.PeerKey...)
	body = append(body, peerNonce...)
	body = append(body, opts.Credentials.Fingerprint...)
	body = append(body, opts.PeerFingerprint...)

	final := append(body, opts.Identity.Sign(join(FinishDomain, exporter, opts.Binding, body))...)
	if _, err := socket.Write(final); err != nil {
		return err
	}

	return readFinish(socket, opts, exporter, nonce, peerNonce)
}

// readHello reads the first proof of the other side.
func readHello(socket *tls.Conn, opts Options, exporter []byte) ([]byte, error) {
	message := make([]byte, 1+32*3+64)
	if _, err := readFull(socket, message); err != nil {
		return nil, err
	}
	if message[0] != helloKind {
		return nil, ErrInvalidProof
	}

	peerKey := message[1:33]
	peerNonce := message[33:65]
	peerFingerprint := message[65:97]
	signature := message[97:]

	if string(peerKey) != string(opts.PeerKey) {
		return nil, ErrPeerKeyMismatch
	}
	if string(peerFingerprint) != string(opts.PeerFingerprint) {
		return nil, ErrCertificateMismatch
	}
	if !identity.Verify(peerKey, join(HelloDomain, exporter, opts.Binding, message[:97]), signature) {
		return nil, ErrInvalidProof
	}
	return peerNonce, nil
}

// readFinish reads the second proof, which names both sides.
func readFinish(socket *tls.Conn, opts Options, exporter, nonce, peerNonce []byte) error {
	message := make([]byte, 1+32*6+64)
	if _, err := readFull(socket, message); err != nil {
		return err
	}
	if message[0] != finishKind {
		return ErrInvalidProof
	}

	body := message[:1+32*6]
	signature := message[1+32*6:]

	if string(body[1:33]) != string(opts.PeerKey) || string(body[33:65]) != string(peerNonce) {
		return ErrMisdirected
	}
	if string(body[65:97]) != string(opts.Identity.PublicKey) || string(body[97:129]) != string(nonce) {
		return ErrMisdirected
	}
	if string(body[129:161]) != string(opts.PeerFingerprint) ||
		string(body[161:193]) != string(opts.Credentials.Fingerprint) {
		return ErrMisdirected
	}
	if !identity.Verify(opts.PeerKey, join(FinishDomain, exporter, opts.Binding, body), signature) {
		return ErrInvalidProof
	}
	return nil
}

// readFull reads one message of a known length.
func readFull(socket *tls.Conn, into []byte) (int, error) {
	read := 0
	for read < len(into) {
		count, err := socket.Read(into[read:])
		if err != nil {
			return read, err
		}
		read += count
	}
	return read, nil
}

func join(domain string, parts ...[]byte) []byte {
	out := []byte(domain)
	for _, part := range parts {
		out = append(out, part...)
	}
	return out
}
