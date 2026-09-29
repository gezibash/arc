package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
)

// SessionRecord maps HTTP metadata/body or a WebSocket message to a core byte
// stream. WebSocket text stays text; binary HTTP/WS payloads are base64 in JSON.
type SessionRecord struct {
	Type     string      `json:"type"`
	Status   int         `json:"status,omitempty"`
	Headers  http.Header `json:"headers,omitempty"`
	Data     []byte      `json:"data,omitempty"`
	Text     string      `json:"text,omitempty"`
	Code     int         `json:"code,omitempty"`
	Reason   string      `json:"reason,omitempty"`
	Protocol string      `json:"protocol,omitempty"`
}

func (a *Adapter) HandleSession(ctx context.Context, req provider.Request, stream *session.Stream) error {
	var in httpRequest
	decoder := json.NewDecoder(strings.NewReader(req.Message))
	decoder.DisallowUnknownFields()
	if req.Message != "" {
		if err := decoder.Decode(&in); err != nil {
			return provider.ErrInvalidRequest
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return provider.ErrInvalidRequest
		}
	}
	request, err := httpRequestOf(ctx, req)
	if err != nil {
		return provider.ErrInvalidRequest
	}
	if in.StreamBody && (stream.Mode() != session.Duplex || in.Body != "" || in.BodyBase64 != "" || in.WebSocket) {
		return provider.ErrInvalidRequest
	}
	if in.WebSocket && (stream.Mode() != session.Duplex || request.Method != http.MethodGet || in.Body != "" || in.BodyBase64 != "") {
		return provider.ErrInvalidRequest
	}
	client, closeClient := handlerClient(ctx, a.handler)
	defer closeClient()
	if in.WebSocket {
		return websocketSession(ctx, client, request, in.Subprotocols, stream)
	}
	if in.StreamBody {
		request.Body = io.NopCloser(stream)
		request.ContentLength = -1
	}
	response, err := client.Do(request)
	if err != nil {
		return provider.Error("http_exchange_failed")
	}
	defer response.Body.Close()
	encoder := json.NewEncoder(stream)
	if err = encoder.Encode(SessionRecord{Type: "response", Status: response.StatusCode, Headers: response.Header}); err != nil {
		return err
	}
	buf := make([]byte, session.MaxChunk)
	for {
		n, readErr := response.Body.Read(buf)
		if n > 0 {
			if err = encoder.Encode(SessionRecord{Type: "body", Data: buf[:n]}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return provider.Error("http_body_failed")
		}
	}
	if len(response.Trailer) > 0 {
		if err = encoder.Encode(SessionRecord{Type: "trailers", Headers: response.Trailer}); err != nil {
			return err
		}
	}
	return encoder.Encode(SessionRecord{Type: "end"})
}

// A private in-memory HTTP connection lets net/http supply real flushing,
// request streaming, and hijacking. No TCP listener or second ARC protocol.
func handlerClient(ctx context.Context, handler http.Handler) (*http.Client, func()) {
	client, server := net.Pipe()
	listener := &singleListener{conn: server, done: make(chan struct{})}
	httpServer := &http.Server{BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 5 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = http.NewResponseController(w).EnableFullDuplex()
		handler.ServeHTTP(w, r)
	})}
	go httpServer.Serve(listener)
	var dial sync.Once
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		var conn net.Conn
		dial.Do(func() { conn = client })
		if conn == nil {
			return nil, errors.New("HTTP session cannot redial")
		}
		return conn, nil
	}}
	cleanup := func() {
		client.Close()
		server.Close()
		httpServer.Close()
		listener.Close()
		transport.CloseIdleConnections()
	}
	return &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cleanup
}

type singleListener struct {
	conn     net.Conn
	mu       sync.Mutex
	accepted bool
	done     chan struct{}
	once     sync.Once
}

func (l *singleListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.accepted {
		l.accepted = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.done
	return nil, net.ErrClosed
}
func (l *singleListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

func websocketSession(ctx context.Context, client *http.Client, request *http.Request, protocols []string, stream *session.Stream) error {
	conn, response, err := websocket.Dial(ctx, request.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: request.Header, Subprotocols: protocols})
	out := json.NewEncoder(stream)
	var mu sync.Mutex
	send := func(v SessionRecord) error { mu.Lock(); defer mu.Unlock(); return out.Encode(v) }
	if err != nil {
		if response != nil {
			_ = send(SessionRecord{Type: "response", Status: response.StatusCode, Headers: response.Header})
		}
		return provider.Error("websocket_handshake_failed")
	}
	defer conn.CloseNow()
	stop := context.AfterFunc(ctx, func() { conn.CloseNow() })
	defer stop()
	conn.SetReadLimit(MaxHTTPBody)
	if err = send(SessionRecord{Type: "response", Status: 101, Headers: response.Header, Protocol: conn.Subprotocol()}); err != nil {
		return err
	}
	inputResult := make(chan error, 1)
	go func() {
		err := websocketInput(ctx, conn, stream, send)
		inputResult <- err
		if err != nil {
			conn.CloseNow()
		}
	}()
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			select {
			case inputErr := <-inputResult:
				if inputErr != nil {
					return inputErr
				}
			default:
			}
			var closed websocket.CloseError
			if errors.As(err, &closed) {
				if err = send(SessionRecord{Type: "ws_close", Code: int(closed.Code), Reason: closed.Reason}); err != nil {
					return err
				}
				if closed.Code == websocket.StatusNormalClosure || closed.Code == websocket.StatusGoingAway {
					return nil
				}
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return provider.Error("websocket_closed")
		}
		record := SessionRecord{Type: "binary", Data: data}
		if kind == websocket.MessageText {
			if !utf8.Valid(data) {
				return provider.Error("invalid_websocket_text")
			}
			record = SessionRecord{Type: "text", Text: string(data)}
		}
		if err = send(record); err != nil {
			return err
		}
	}
}
func websocketInput(ctx context.Context, conn *websocket.Conn, stream *session.Stream, send func(SessionRecord) error) error {
	// Limit each complete application message independently of transport chunks.
	reader := NewSessionReader(stream)
	for {
		record, err := reader.Next()
		if err == io.EOF {
			return conn.Close(websocket.StatusNormalClosure, "")
		}
		if err != nil {
			return err
		}
		switch record.Type {
		case "text":
			err = conn.Write(ctx, websocket.MessageText, []byte(record.Text))
		case "binary":
			err = conn.Write(ctx, websocket.MessageBinary, record.Data)
		case "ping":
			err = conn.Ping(ctx)
			if err == nil {
				err = send(SessionRecord{Type: "pong"})
			}
		case "close":
			code := record.Code
			if code == 0 {
				code = 1000
			}
			if len(record.Reason) > 123 || !utf8.ValidString(record.Reason) {
				return provider.ErrInvalidRequest
			}
			return conn.Close(websocket.StatusCode(code), record.Reason)
		default:
			return provider.ErrInvalidRequest
		}
		if err != nil {
			return err
		}
	}
}
