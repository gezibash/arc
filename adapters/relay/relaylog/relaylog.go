// Package relaylog makes a khatru relay log what happens to its client
// connections, so that an operator can read the health of the relay from
// its own log.
//
// A healthy relay logs little. The log has a WARN line for each failed
// WebSocket upgrade, for each connection that closes with an error, such as
// a reset or a timeout, and for each refused event, and an INFO summary each
// interval in which something happened. A normal request and a normal close
// have no line. A gift wrap that comes before NIP-42 authentication is the
// normal first step of the handshake, so its refusal has a DEBUG line. The
// summary counts it. The log never holds event contents, keys or IP
// addresses.
package relaylog

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fiatjaf.com/nostr/khatru"

	"github.com/gezibash/arc/adapters/relay/limits"
)

// MaxRefusalLines is the number of refusal lines for one reason in one
// interval. The summary counts the refusals above it, so that a flood does
// not flood the log.
const MaxRefusalLines = 5

// Log counts and logs the connections of one relay.
type Log struct {
	logger *slog.Logger

	open atomic.Int64

	mu       sync.Mutex
	opened   int64
	closed   int64
	failed   int64 // connections that closed with an error
	eof      int64 // connections that closed without a close frame
	upgrades int64 // failed upgrades
	refused  map[string]int64
}

type connKey struct{}

// New makes a Log that writes to logger.
func New(logger *slog.Logger) *Log {
	return &Log{logger: logger, refused: map[string]int64{}}
}

// Watch makes the relay log to l. Call it after the other changes to the
// relay. The server of the relay must accept its connections through
// Listen, and set its ConnContext to the ConnContext of l.
func (l *Log) Watch(rl *khatru.Relay) {
	rl.Log = log.New(bridge{l}, "", 0)
	onConnect, onDisconnect := rl.OnConnect, rl.OnDisconnect
	rl.OnConnect = func(ctx context.Context) {
		if c := connOf(ctx); c != nil {
			c.start()
		}
		l.open.Add(1)
		l.mu.Lock()
		l.opened++
		l.mu.Unlock()
		if onConnect != nil {
			onConnect(ctx)
		}
	}
	rl.OnDisconnect = func(ctx context.Context) {
		if onDisconnect != nil {
			onDisconnect(ctx)
		}
		l.open.Add(-1)
		var (
			err      error
			duration time.Duration
		)
		if c := connOf(ctx); c != nil {
			duration, err = c.stop()
		}
		// Clients often close the TCP connection without a close frame,
		// so an EOF is no error. A peer that goes away without a FIN
		// shows as a timeout or a reset.
		eof := errors.Is(err, io.EOF)
		if eof {
			err = nil
		}
		l.mu.Lock()
		l.closed++
		if eof {
			l.eof++
		}
		if err != nil {
			l.failed++
		}
		l.mu.Unlock()
		if err != nil {
			l.logger.Warn("connection closed with an error", "duration", duration.Round(time.Millisecond), "reason", reason(err))
		}
	}
}

// Listen returns a listener whose connections record their first error
// after the WebSocket upgrade.
func (l *Log) Listen(listener net.Listener) net.Listener {
	return watchedListener{listener}
}

// ConnContext gives each request the connection that carries it. Set it as
// the ConnContext of the http.Server.
func (l *Log) ConnContext(ctx context.Context, c net.Conn) context.Context {
	if w, ok := c.(*conn); ok {
		return context.WithValue(ctx, connKey{}, w)
	}
	return ctx
}

// Refused logs a refused event. Set it as the Refused function of the
// limits policy.
func (l *Log) Refused(reason string) {
	l.mu.Lock()
	l.refused[reason]++
	n := l.refused[reason]
	l.mu.Unlock()
	if n > MaxRefusalLines {
		return
	}
	level := slog.LevelWarn
	if reason == limits.RefusedWrapAuth {
		level = slog.LevelDebug
	}
	l.logger.Log(context.Background(), level, "event refused", "reason", reason)
}

// Run logs a summary each interval in which something happened, until ctx
// ends.
func (l *Log) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.summary()
		}
	}
}

func (l *Log) summary() {
	l.mu.Lock()
	attrs := []any{"open", l.open.Load(), "opened", l.opened, "closed", l.closed, "closed_without_frame", l.eof, "closed_with_error", l.failed, "failed_upgrades", l.upgrades}
	active := l.opened+l.closed+l.upgrades > 0
	for reason, n := range l.refused {
		attrs = append(attrs, "refused_"+reason, n)
		active = true
	}
	l.opened, l.closed, l.eof, l.failed, l.upgrades = 0, 0, 0, 0, 0
	clear(l.refused)
	l.mu.Unlock()
	if active {
		l.logger.Info("relay summary", attrs...)
	}
}

// bridge turns the three lines that khatru logs into slog lines. One khatru
// line holds an IP address, which the bridge drops.
type bridge struct{ l *Log }

func (b bridge) Write(p []byte) (int, error) {
	line := strings.TrimSpace(string(p))
	switch {
	case strings.HasPrefix(line, "failed to upgrade websocket: "):
		b.l.mu.Lock()
		b.l.upgrades++
		b.l.mu.Unlock()
		b.l.logger.Warn("websocket upgrade failed", "reason", strings.TrimPrefix(line, "failed to upgrade websocket: "))
	case strings.HasPrefix(line, "unexpected close error from "):
		// "unexpected close error from <ip>: <error>"
		_, err, _ := strings.Cut(strings.TrimPrefix(line, "unexpected close error from "), ": ")
		b.l.logger.Warn("connection closed with an unexpected close code", "reason", err)
	case strings.HasPrefix(line, "error writing ping: "):
		// The connection records the write error, and its close line
		// reports it.
	default:
		b.l.logger.Warn("khatru", "message", line)
	}
	return len(p), nil
}

// reason is a short reason for the error that closed a connection.
func reason(err error) string {
	var netErr net.Error
	switch {
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout: " + err.Error()
	}
	return err.Error()
}

func connOf(ctx context.Context) *conn {
	r := khatru.GetRequest(ctx)
	if r == nil {
		return nil
	}
	c, _ := r.Context().Value(connKey{}).(*conn)
	return c
}

type watchedListener struct{ net.Listener }

func (w watchedListener) Accept() (net.Conn, error) {
	c, err := w.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &conn{Conn: c, accepted: time.Now()}, nil
}

// conn records the first read or write error after the upgrade. The HTTP
// server stops its own read with an error when it gives the connection to
// the WebSocket, so the record starts only at the upgrade.
type conn struct {
	net.Conn
	// accepted is when the listener took the connection. The duration of
	// a connection counts from it: the upgrade comes after the reply that
	// a client already reads, so a duration from the upgrade could be
	// shorter than the connection was open for the client (#454).
	accepted time.Time
	mu       sync.Mutex
	started  bool
	err      error
	closing  bool // the relay closed the connection
}

func (c *conn) start() {
	c.mu.Lock()
	c.started = true
	c.mu.Unlock()
}

// stop returns the duration of the connection, and the error that closed it.
func (c *conn) stop() (time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closing = true
	return time.Since(c.accepted), c.err
}

func (c *conn) record(err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.started && !c.closing && c.err == nil {
		c.err = err
	}
	c.mu.Unlock()
}

func (c *conn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.record(err)
	return n, err
}

func (c *conn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.record(err)
	return n, err
}

// CloseWrite keeps the half-close of a TCP connection, which the HTTP server
// uses to end a plain request.
func (c *conn) CloseWrite() error {
	if w, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return w.CloseWrite()
	}
	return nil
}
