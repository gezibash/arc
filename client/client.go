// Package client connects one identity to a relay.
//
// A client opens one TCP connection, proves its identity, and then:
//
//   - announces the capabilities that it offers,
//   - resolves a petname or a key prefix to an announcement,
//   - searches the directory,
//   - sends packets to another citizen, and reads the packets that arrive.
//
// The relay never holds a key of the citizen, and never reads a packet body.
package client

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/canonical"
	"github.com/gezibash/arc/internal/wire"
)

// The timeouts of the directory, which follow the Elixir implementation.
const (
	DefaultDialTimeout      = 10 * time.Second
	DefaultDirectoryTimeout = 2 * time.Second
	SearchTimeout           = 10 * time.Second
	StatusTimeout           = 5 * time.Second
	// PacketBuffer is the number of inbound packets that wait for a reader.
	PacketBuffer = 64
)

// Errors of this package.
var (
	ErrClosed        = errors.New("client: the connection is closed")
	ErrRelayMismatch = errors.New("client: the relay is not the pinned relay")
	ErrFrameTooLarge = wire.ErrFrameTooLarge
)

// DirectoryError reports a directory request that the relay refused.
type DirectoryError struct {
	Operation string
	Reason    string
}

func (e *DirectoryError) Error() string {
	return fmt.Sprintf("client: the relay refused the %s: %s", e.Operation, e.Reason)
}

// Options holds what a client needs to connect.
type Options struct {
	// Identity is the local identity. It is needed.
	Identity *identity.Identity
	// RelayPublicKey pins the relay. A relay with another key is refused.
	RelayPublicKey []byte
	// DialTimeout bounds the TCP connect and the handshake.
	DialTimeout time.Duration
	// Waker wakes a citizen whose machine may pause, before a request goes
	// to it. Without one, every request goes out at once.
	Waker Waker
}

// Waker makes a paused citizen ready for a request. The wake package runs
// the wake hooks of the caller. The wake counts toward the deadline of the
// request.
type Waker interface {
	// Wake returns when the citizen is ready to answer, or with the reason
	// it is not.
	Wake(ctx context.Context, citizen []byte) error
	// Answered records an answer from the citizen. An answer proves that
	// the citizen is awake.
	Answered(citizen []byte)
}

// Client is one connection to one relay.
type Client struct {
	conn     net.Conn
	me       *identity.Identity
	relayKey []byte
	waker    Waker

	packets chan []byte
	done    chan struct{}

	mu       sync.Mutex
	pending  map[string]chan map[string]any
	maxFrame uint32
	closed   bool
	reason   error
}

// Dial opens a connection to a relay and proves the identity of this citizen.
func Dial(ctx context.Context, address string, opts Options) (*Client, error) {
	if opts.Identity == nil {
		return nil, errors.New("client: an identity is needed")
	}

	timeout := opts.DialTimeout
	if timeout == 0 {
		timeout = DefaultDialTimeout
	}

	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	relayKey, challenge, err := wire.ReadRelayHello(conn)
	if err != nil {
		conn.Close()
		return nil, err
	}

	if len(opts.RelayPublicKey) > 0 && !equal(relayKey, opts.RelayPublicKey) {
		conn.Close()
		return nil, fmt.Errorf("%w: the relay is %s", ErrRelayMismatch, identity.Name(relayKey))
	}

	hello, err := wire.ClientHello(opts.Identity, relayKey, challenge)
	if err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := conn.Write(hello); err != nil {
		conn.Close()
		return nil, err
	}

	_ = conn.SetDeadline(time.Time{})

	client := &Client{
		conn:     conn,
		me:       opts.Identity,
		relayKey: relayKey,
		waker:    opts.Waker,
		packets:  make(chan []byte, PacketBuffer),
		done:     make(chan struct{}),
		pending:  map[string]chan map[string]any{},
	}

	go client.read()
	return client, nil
}

// RelayPublicKey returns the key that the relay proved at the handshake.
func (c *Client) RelayPublicKey() []byte { return c.relayKey }

// Identity returns the local identity.
func (c *Client) Identity() *identity.Identity { return c.me }

// Packets returns the packets that arrive for this citizen. The channel
// closes when the connection ends.
func (c *Client) Packets() <-chan []byte { return c.packets }

// Done closes when the connection ends. Err then says why.
func (c *Client) Done() <-chan struct{} { return c.done }

// Err returns the reason the connection ended, or nil while it runs.
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.reason
}

// Close ends the connection.
func (c *Client) Close() error {
	c.stop(ErrClosed)
	return nil
}

// SendPacket writes one ARC packet to the relay, which forwards it.
func (c *Client) SendPacket(raw []byte) error {
	c.mu.Lock()
	closed, max := c.closed, c.maxFrame
	c.mu.Unlock()

	if closed {
		return ErrClosed
	}
	if max > 0 && uint32(len(raw)) > max {
		return fmt.Errorf("%w: the relay accepts %d bytes", ErrFrameTooLarge, max)
	}
	return wire.WriteFrame(c.conn, raw)
}

// Announce publishes a signed announcement. The record comes from the
// announce package.
func (c *Client) Announce(ctx context.Context, record map[string]any) error {
	_, err := c.directory(ctx, "announce", map[string]any{"record": record}, DefaultDirectoryTimeout)
	return err
}

// Resolve finds the citizens that answer to a petname or to the start of a
// public key. It returns the signed announcement of each one.
func (c *Client) Resolve(ctx context.Context, query string) ([]map[string]any, error) {
	reply, err := c.directory(ctx, "resolve", map[string]any{"query": query}, SearchTimeout)
	if err != nil {
		return nil, err
	}
	return records(reply["entries"]), nil
}

// Page is one page of a search.
type Page struct {
	// Entries holds the signed announcement of each citizen on this page.
	Entries []map[string]any
	// Next is the cursor of the next page, or the empty string at the end.
	Next string
	// Total is the number of entries that match the query.
	Total int
}

// Search asks the directory for the citizens that offer a capability. A limit
// of zero takes the default of the relay. An after of "" starts at the front.
func (c *Client) Search(ctx context.Context, query string, limit int, after string) (*Page, error) {
	fields := map[string]any{"query": query}
	if limit > 0 {
		fields["limit"] = limit
	}
	if after != "" {
		fields["after"] = after
	}

	reply, err := c.directory(ctx, "search", fields, SearchTimeout)
	if err != nil {
		return nil, err
	}

	page := &Page{Entries: records(reply["entries"])}
	if next, ok := reply["next"].(string); ok {
		page.Next = next
	}
	if total, ok := reply["total"].(json.Number); ok {
		if value, err := total.Int64(); err == nil {
			page.Total = int(value)
		}
	}
	return page, nil
}

// Endpoint is an address as another party sees it.
type Endpoint struct {
	Host string
	Port int
}

// Observe asks the relay for the address that this connection comes from.
func (c *Client) Observe(ctx context.Context) (Endpoint, error) {
	reply, err := c.directory(ctx, "observe", nil, DefaultDirectoryTimeout)
	if err != nil {
		return Endpoint{}, err
	}

	observed, _ := reply["observed"].(map[string]any)
	host, _ := observed["host"].(string)
	port, _ := observed["port"].(json.Number)
	number, _ := port.Int64()
	return Endpoint{Host: host, Port: int(number)}, nil
}

// Status asks the relay about itself.
func (c *Client) Status(ctx context.Context) (map[string]any, error) {
	reply, err := c.directory(ctx, "status", nil, StatusTimeout)
	if err != nil {
		return nil, err
	}

	status, _ := reply["status"].(map[string]any)
	return status, nil
}

// directory sends one control request and waits for the reply that carries the
// same request id.
func (c *Client) directory(ctx context.Context, operation string, fields map[string]any, timeout time.Duration) (map[string]any, error) {
	requestID, err := newRequestID()
	if err != nil {
		return nil, err
	}

	request := map[string]any{"type": operation, "request_id": requestID}
	for key, value := range fields {
		request[key] = value
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	if len(payload) > wire.MaxDirectoryControlBytes {
		return nil, fmt.Errorf("client: the %s is over %d bytes", operation, wire.MaxDirectoryControlBytes)
	}

	replies := make(chan map[string]any, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrClosed
	}
	c.pending[requestID] = replies
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		delete(c.pending, requestID)
		c.mu.Unlock()
	}()

	if err := wire.WriteFrame(c.conn, wire.Directory(payload)); err != nil {
		return nil, err
	}

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case reply := <-replies:
		if ok, _ := reply["ok"].(bool); !ok {
			reason, _ := reply["error"].(string)
			if reason == "" {
				reason = "unknown"
			}
			return nil, &DirectoryError{Operation: operation, Reason: reason}
		}
		return reply, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		return nil, fmt.Errorf("client: the relay did not answer the %s", operation)
	case <-c.done:
		return nil, c.Err()
	}
}

// read is the one reader of the connection. It runs until the connection ends.
func (c *Client) read() {
	defer close(c.packets)

	for {
		payload, err := wire.ReadFrame(c.conn, 0)
		if err != nil {
			c.stop(err)
			return
		}

		if control, ok := wire.IsDirectory(payload); ok {
			c.deliverControl(control)
			continue
		}
		if max, ok := wire.DecodeRelayInfo(payload); ok {
			c.mu.Lock()
			c.maxFrame = max
			c.mu.Unlock()
			continue
		}

		select {
		case c.packets <- payload:
		case <-c.done:
			return
		}
	}
}

func (c *Client) deliverControl(payload []byte) {
	// The numbers keep their digits, because a signed announcement travels
	// inside this reply and its signature covers the canonical bytes.
	value, err := canonical.Decode(payload)
	if err != nil {
		return
	}
	reply, ok := value.(map[string]any)
	if !ok {
		return
	}
	if kind, _ := reply["type"].(string); kind != "reply" {
		return
	}

	requestID, ok := reply["request_id"].(string)
	if !ok {
		return
	}

	c.mu.Lock()
	waiting := c.pending[requestID]
	delete(c.pending, requestID)
	c.mu.Unlock()

	if waiting != nil {
		waiting <- reply
	}
}

func (c *Client) stop(reason error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	c.reason = reason
	c.mu.Unlock()

	close(c.done)
	c.conn.Close()
}

func newRequestID() (string, error) {
	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", err
	}
	return hex.EncodeToString(id), nil
}

func records(value any) []map[string]any {
	list, _ := value.([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if record, ok := item.(map[string]any); ok {
			out = append(out, record)
		}
	}
	return out
}

func equal(left, right []byte) bool {
	return len(left) == len(right) && string(left) == string(right)
}
