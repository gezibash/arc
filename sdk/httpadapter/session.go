package httpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
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
	// SDP is the answer of a pipe, in a "direct" record.
	SDP string `json:"sdp,omitempty"`
	// BodyBytes is the number of body bytes that went on the pipe, in the
	// "end" record of a session with a pipe.
	BodyBytes *int64 `json:"body_bytes,omitempty"`
}

// The errors of a session with a pipe. See docs/http/SPEC.md, section 3.7.
const (
	// ErrNoPipe says that the provider opens no pipe. The consumer can ask
	// again with no offer.
	ErrNoPipe = provider.Error("no_pipe")
	// ErrPipesBusy says that the provider holds direct.MaxPipes pipes.
	ErrPipesBusy = provider.Error("busy")
	// ErrNoPath says that the data channel of the pipe did not open.
	ErrNoPath = provider.Error("no_path")
	// ErrPipeFailed says that the pipe closed before the end of the body.
	ErrPipeFailed = provider.Error("pipe_failed")
)

func (a *Adapter) HandleSession(ctx context.Context, req provider.Request, stream *session.Stream) error {
	in, err := decodeHTTPRequest(req.Message)
	if err != nil {
		return provider.ErrInvalidRequest
	}
	request, err := httpRequestOf(ctx, req, in)
	if err != nil {
		return provider.ErrInvalidRequest
	}
	if in.StreamBody && (stream.Mode() != session.Duplex || in.Body != "" || in.BodyBase64 != "" || in.WebSocket) {
		return provider.ErrInvalidRequest
	}
	if in.WebSocket && (stream.Mode() != session.Duplex || request.Method != http.MethodGet || in.Body != "" || in.BodyBase64 != "") {
		return provider.ErrInvalidRequest
	}
	if in.Direct != nil && (in.WebSocket || in.StreamBody) {
		return provider.ErrInvalidRequest
	}
	encoder := json.NewEncoder(stream)
	var pipe *direct.Pipe
	if in.Direct != nil {
		release, err := a.takePipe()
		if err != nil {
			return err
		}
		defer release()
		if pipe, err = a.openPipe(ctx, *in.Direct, encoder); err != nil {
			return err
		}
		defer func() { _ = pipe.Close() }()
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
	defer func() { _ = response.Body.Close() }()
	if err = encoder.Encode(SessionRecord{Type: "response", Status: response.StatusCode, Headers: response.Header}); err != nil {
		return err
	}
	if pipe != nil {
		return pipeBody(ctx, pipe, response, encoder)
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

// takePipe takes a slot for one pipe. It refuses an offer when the policy
// opens no pipe, and when the slots are full.
func (a *Adapter) takePipe() (func(), error) {
	if a.pipes.Policy == direct.Off || a.slots == nil {
		return nil, ErrNoPipe
	}
	select {
	case a.slots <- struct{}{}:
		return func() { <-a.slots }, nil
	default:
		return nil, ErrPipesBusy
	}
}

// openPipe answers the offer with a "direct" record, and waits for the
// data channel. The handler has not run yet, so a failed attempt has no
// effect.
func (a *Adapter) openPipe(ctx context.Context, offer direct.Offer, encoder *json.Encoder) (*direct.Pipe, error) {
	if offer.Check() != nil {
		return nil, provider.ErrInvalidRequest
	}
	pipe, answer, err := direct.Accept(ctx, a.pipes, offer)
	if err != nil {
		return nil, ErrNoPath
	}
	if err = encoder.Encode(SessionRecord{Type: "direct", SDP: answer}); err != nil {
		_ = pipe.Close()
		return nil, err
	}
	if err = pipe.Wait(ctx); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrNoPath
	}
	return pipe, nil
}

// pipeBody copies the body to the pipe, then sends the trailers and an
// "end" record with the number of body bytes.
func pipeBody(ctx context.Context, pipe *direct.Pipe, response *http.Response, encoder *json.Encoder) error {
	n, err := io.Copy(pipe, response.Body)
	if errors.Is(err, direct.ErrPipeClosed) {
		return ErrPipeFailed
	}
	if err != nil {
		return provider.Error("http_body_failed")
	}
	if err = pipe.CloseWrite(ctx); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrPipeFailed
	}
	if len(response.Trailer) > 0 {
		if err = encoder.Encode(SessionRecord{Type: "trailers", Headers: response.Trailer}); err != nil {
			return err
		}
	}
	return encoder.Encode(SessionRecord{Type: "end", BodyBytes: &n})
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
	go func() { _ = httpServer.Serve(listener) }()
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
		_ = client.Close()
		_ = server.Close()
		_ = httpServer.Close()
		_ = listener.Close()
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
	conn, response, err := websocket.Dial(ctx, request.URL.String(), &websocket.DialOptions{HTTPClient: client, HTTPHeader: request.Header, Subprotocols: protocols}) //nolint:bodyclose // websocket.Dial owns the body; its docs say the caller never closes it.
	out := json.NewEncoder(stream)
	var mu sync.Mutex
	send := func(v SessionRecord) error { mu.Lock(); defer mu.Unlock(); return out.Encode(v) }
	if err != nil {
		if response != nil {
			_ = send(SessionRecord{Type: "response", Status: response.StatusCode, Headers: response.Header})
		}
		return provider.Error("websocket_handshake_failed")
	}
	defer func() { _ = conn.CloseNow() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.CloseNow() })
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
			_ = conn.CloseNow()
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
			if closed, ok := errors.AsType[websocket.CloseError](err); ok {
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
		if errors.Is(err, io.EOF) {
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
