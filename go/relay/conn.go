package relay

import (
	"crypto/rand"
	"log/slog"
	"net"
	"sync"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/wire"
)

// conn is one connection of one citizen.
//
// A writer goroutine owns the socket for writing, so two answers never
// interleave. The queue bounds how far one slow reader may fall behind: when
// the queue is full, the relay drops the packet and keeps serving the others.
type conn struct {
	socket    net.Conn //nolint:structcheck // the federation reads it
	publicKey []byte
	log       *slog.Logger

	queue chan []byte

	once   sync.Once
	closed chan struct{}
}

func newConn(socket net.Conn, publicKey []byte, log *slog.Logger) *conn {
	return &conn{
		socket:    socket,
		publicKey: publicKey,
		log:       log,
		queue:     make(chan []byte, SendQueue),
		closed:    make(chan struct{}),
	}
}

// send puts one frame in the queue. It returns false when the queue is full.
func (c *conn) send(payload []byte) bool {
	select {
	case c.queue <- payload:
		return true
	case <-c.closed:
		return false
	default:
		return false
	}
}

// write owns the socket for writing.
func (c *conn) write() {
	for {
		select {
		case payload := <-c.queue:
			if err := wire.WriteFrame(c.socket, payload); err != nil {
				c.log.Debug("the write failed", "citizen", identity.Name(c.publicKey), "error", err)
				c.close()
				return
			}
		case <-c.closed:
			return
		}
	}
}

func (c *conn) close() {
	c.once.Do(func() {
		close(c.closed)
		c.socket.Close()
	})
}

func (c *conn) endpoint() (string, int) {
	address, ok := c.socket.RemoteAddr().(*net.TCPAddr)
	if !ok {
		return "", 0
	}
	return address.IP.String(), address.Port
}

func randRead(into []byte) (int, error) { return rand.Read(into) }
