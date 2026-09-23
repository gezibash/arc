// Package provider holds the runtime that serves one capability.
//
// ARC starts the provider as a program, and speaks newline delimited JSON on
// its standard input and its standard output. One line carries one event.
//
// ARC sends:
//
//	{"op":"request","request_id":"...","from":"<hex>","message":"...","meta":{...},
//	 "arc_session_id":"...","app_session_id":null,"framed":true}
//
// The provider answers:
//
//	{"op":"reply","request_id":"...","reply":"..."}
//	{"op":"error","request_id":"...","error":"invalid_request"}
//
// A provider can call a capability that the citizen who serves it installed.
// The call goes out as that citizen. The provider writes:
//
//	{"op":"call","call_id":"...","address":"sqlite+arc://<key>/main","body":"..."}
//
// ARC makes the call, and writes one result with the same call_id:
//
//	{"op":"result","call_id":"...","reply":"..."}
//	{"op":"result","call_id":"...","refused":"invalid_request"}
//	{"op":"result","call_id":"...","error":"..."}
//
// refused is the error of the provider that got the call. error says why ARC
// could not make the call.
//
// Standard output carries the protocol, so a provider writes its logs to
// standard error and never to standard output.
//
// A provider in full:
//
//	type echo struct{}
//
//	func (echo) HandleRequest(_ context.Context, r provider.Request) (string, error) {
//	    return r.Message, nil
//	}
//
//	func main() {
//	    provider.Run(context.Background(), echo{}, provider.Options{})
//	}
package provider

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"sync"
)

// DefaultMaxLineBytes caps one input line. A JSON string grows about six
// times when it carries escaped text, so the cap stands well above the body
// limit of a request.
const DefaultMaxLineBytes = 8 * 1024 * 1024

// Request is one event from ARC.
type Request struct {
	// Op is the kind of event, for example "request".
	Op string
	// From is the public key of the caller, in lower case hex.
	From string
	// Message is the body of the request.
	Message string
	// Meta holds the frame meta, for example the method and the path.
	Meta map[string]any
	// RequestID correlates the answer. It goes back unchanged.
	RequestID any
	// ArcSessionID names the encrypted session, in hex.
	ArcSessionID string
	// AppSessionID names a stream of the application.
	AppSessionID string
	// Framed says whether the caller used the frame protocol.
	Framed bool
}

// Method returns meta.method, which the capability manifest sets.
func (r Request) Method() string {
	method, _ := r.Meta["method"].(string)
	return method
}

// Path returns meta.path, which the caller sets in the URI.
func (r Request) Path() string {
	path, _ := r.Meta["path"].(string)
	return path
}

// Caller calls other capabilities, as the citizen that serves this provider.
type Caller interface {
	// Call sends one live call to the capability that an address names,
	// <scheme>+arc://<provider>/<path>, and returns the reply. The method is
	// the one that the manifest of the capability names.
	Call(ctx context.Context, address, body string) (string, error)
}

// WantsCaller is a handler that calls other capabilities. Run gives it the
// caller before it serves the first request.
type WantsCaller interface {
	SetCaller(caller Caller)
}

// CallError is a call that got no reply.
type CallError struct {
	// Refused says that the provider that got the call answered with an
	// error. Otherwise ARC could not make the call.
	Refused bool
	// Reason is the error of that provider, or the reason of ARC.
	Reason string
}

func (e *CallError) Error() string {
	if e.Refused {
		return "provider: the call was refused: " + e.Reason
	}
	return "provider: the call failed: " + e.Reason
}

// Handler answers one request. Several requests run at one time, so a handler
// must be safe for several goroutines.
type Handler interface {
	HandleRequest(ctx context.Context, request Request) (string, error)
}

// HandlerFunc turns one function into a Handler.
type HandlerFunc func(ctx context.Context, request Request) (string, error)

// HandleRequest calls the function.
func (f HandlerFunc) HandleRequest(ctx context.Context, request Request) (string, error) {
	return f(ctx, request)
}

// Error is an error whose text is safe to send to the caller. Every other
// error answers "internal_error", and the detail goes to standard error.
type Error string

// Error returns the code that the caller sees.
func (e Error) Error() string { return string(e) }

// The errors that the runtime itself answers.
const (
	ErrInvalidRequest  = Error("invalid_request")
	ErrRequestTooLarge = Error("request_too_large")
	ErrInternal        = Error("internal_error")
)

// Options changes how the runtime runs.
type Options struct {
	// In is the source of events. The default is standard input.
	In io.Reader
	// Out is where answers go. The default is standard output.
	Out io.Writer
	// Log is where the runtime reports what it drops. The default is
	// standard error.
	Log io.Writer
	// MaxLineBytes caps one input line. The default is DefaultMaxLineBytes.
	MaxLineBytes int
}

// Run reads events and writes answers until the input ends or the context is
// done. Each request runs in its own goroutine, so one slow request never
// holds up another caller.
func Run(ctx context.Context, handler Handler, opts Options) error {
	if opts.In == nil {
		opts.In = os.Stdin
	}
	if opts.Out == nil {
		opts.Out = os.Stdout
	}
	if opts.Log == nil {
		opts.Log = os.Stderr
	}
	if opts.MaxLineBytes <= 0 {
		opts.MaxLineBytes = DefaultMaxLineBytes
	}

	runtime := &runtime{
		handler: handler, options: opts, out: bufio.NewWriter(opts.Out),
		calls: map[string]chan result{}, stopped: make(chan struct{}),
	}
	if wants, ok := handler.(WantsCaller); ok {
		wants.SetCaller(runtime)
	}
	return runtime.run(ctx)
}

type runtime struct {
	handler Handler
	options Options

	mu  sync.Mutex
	out *bufio.Writer

	group sync.WaitGroup

	// calls holds each call that waits for its result, by call_id.
	callsMu sync.Mutex
	calls   map[string]chan result
	next    uint64
	// stopped closes when the input ends. No result can come after it.
	stopped chan struct{}
}

// result is the answer of ARC to one call.
type result struct {
	reply   *string
	refused string
	err     string
}

func (r *runtime) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	reader := bufio.NewReaderSize(r.options.In, 64*1024)
	defer r.group.Wait()
	// A call that waits when the input ends fails, so its request ends too.
	defer close(r.stopped)

	for {
		line, err := readLine(reader, r.options.MaxLineBytes)
		switch {
		case errors.Is(err, errLineTooLong):
			r.answer(nil, "", ErrRequestTooLarge)
			continue
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}

		if len(line) == 0 {
			continue
		}

		r.group.Add(1)
		go func() {
			defer r.group.Done()
			r.serve(ctx, line)
		}()

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}
}

// serve answers one line. A panic in the handler fails that one request, and
// the provider keeps serving the others.
func (r *runtime) serve(ctx context.Context, line []byte) {
	var event struct {
		Op           string         `json:"op"`
		From         string         `json:"from"`
		Message      *string        `json:"message"`
		Meta         map[string]any `json:"meta"`
		RequestID    any            `json:"request_id"`
		ArcSessionID string         `json:"arc_session_id"`
		AppSessionID string         `json:"app_session_id"`
		Framed       bool           `json:"framed"`

		CallID  string  `json:"call_id"`
		Reply   *string `json:"reply"`
		Refused string  `json:"refused"`
		Error   string  `json:"error"`
	}

	if err := json.Unmarshal(line, &event); err != nil {
		r.answer(nil, "", ErrInvalidRequest)
		return
	}
	if event.Op == "result" {
		r.settle(event.CallID, result{reply: event.Reply, refused: event.Refused, err: event.Error})
		return
	}
	if event.Op != "request" || event.Message == nil || event.Meta == nil || !validRequestID(event.RequestID) {
		r.answer(event.RequestID, "", ErrInvalidRequest)
		return
	}

	request := Request{
		Op:           event.Op,
		From:         event.From,
		Message:      *event.Message,
		Meta:         event.Meta,
		RequestID:    event.RequestID,
		ArcSessionID: event.ArcSessionID,
		AppSessionID: event.AppSessionID,
		Framed:       event.Framed,
	}

	reply, err := r.call(ctx, request)
	r.answer(event.RequestID, reply, err)
}

func (r *runtime) call(ctx context.Context, request Request) (reply string, err error) {
	defer func() {
		if panicked := recover(); panicked != nil {
			fmt.Fprintf(r.options.Log, "provider: the handler panicked: %v\n", panicked)
			reply, err = "", ErrInternal
		}
	}()

	return r.handler.HandleRequest(ctx, request)
}

// Call writes one call line, and waits for its result.
func (r *runtime) Call(ctx context.Context, address, body string) (string, error) {
	answer := make(chan result, 1)
	r.callsMu.Lock()
	r.next++
	id := strconv.FormatUint(r.next, 10)
	r.calls[id] = answer
	r.callsMu.Unlock()
	defer func() {
		r.callsMu.Lock()
		delete(r.calls, id)
		r.callsMu.Unlock()
	}()

	line, err := json.Marshal(map[string]any{"op": "call", "call_id": id, "address": address, "body": body})
	if err != nil {
		return "", err
	}
	if err := r.writeLine(line); err != nil {
		return "", err
	}

	select {
	case got := <-answer:
		return got.outcome()
	case <-r.stopped:
		// A result that came just before the end still counts.
		select {
		case got := <-answer:
			return got.outcome()
		default:
		}
		return "", &CallError{Reason: "the input ended before the result came"}
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (got result) outcome() (string, error) {
	switch {
	case got.refused != "":
		return "", &CallError{Refused: true, Reason: got.refused}
	case got.reply != nil:
		return *got.reply, nil
	case got.err != "":
		return "", &CallError{Reason: got.err}
	}
	return "", &CallError{Reason: "the result holds no reply"}
}

// settle passes a result to the call that waits for it. A result for a call
// that stopped waiting goes.
func (r *runtime) settle(id string, got result) {
	r.callsMu.Lock()
	answer := r.calls[id]
	delete(r.calls, id)
	r.callsMu.Unlock()

	if answer == nil {
		fmt.Fprintf(r.options.Log, "provider: a result came for call %q, which does not wait\n", id)
		return
	}
	answer <- got
}

// answer writes one line. One writer holds the lock, so two answers never
// interleave on one line.
func (r *runtime) answer(requestID any, reply string, err error) {
	response := map[string]any{"request_id": requestID}
	if err != nil {
		response["op"] = "error"
		response["error"] = safeError(r.options.Log, err)
	} else {
		response["op"] = "reply"
		response["reply"] = reply
	}

	line, marshalErr := json.Marshal(response)
	if marshalErr != nil {
		fmt.Fprintf(r.options.Log, "provider: the answer does not encode: %v\n", marshalErr)
		return
	}
	if err := r.writeLine(line); err != nil {
		fmt.Fprintf(r.options.Log, "provider: the answer did not reach ARC: %v\n", err)
	}
}

// writeLine writes one line under the lock, so two answers never share a line.
func (r *runtime) writeLine(line []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.out.Write(line)
	r.out.WriteByte('\n')
	return r.out.Flush()
}

// safeError keeps the detail of an unexpected error out of the answer. The
// caller gets a code, and the operator gets the detail on standard error.
func safeError(log io.Writer, err error) string {
	var safe Error
	if errors.As(err, &safe) {
		return string(safe)
	}
	fmt.Fprintf(log, "provider: %v\n", err)
	return string(ErrInternal)
}

// validRequestID follows the Elixir runtime: a string or a whole number, and
// never a boolean.
func validRequestID(value any) bool {
	switch value := value.(type) {
	case string:
		return true
	case float64:
		return value == float64(int64(value))
	default:
		return false
	}
}

var errLineTooLong = errors.New("provider: the line is too long")

// readLine reads one line. A line over the cap is dropped to its end, so the
// next line still parses.
func readLine(reader *bufio.Reader, max int) ([]byte, error) {
	var line []byte

	for {
		chunk, err := reader.ReadSlice('\n')
		line = append(line, chunk...)

		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > max {
				if err := drain(reader); err != nil {
					return nil, err
				}
				return nil, errLineTooLong
			}
			continue
		}
		if err != nil {
			if len(line) > 0 && errors.Is(err, io.EOF) {
				return trim(line), nil
			}
			return nil, err
		}
		if len(line) > max {
			return nil, errLineTooLong
		}
		return trim(line), nil
	}
}

func drain(reader *bufio.Reader) error {
	for {
		_, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return err
	}
}

func trim(line []byte) []byte {
	for len(line) > 0 && (line[len(line)-1] == '\n' || line[len(line)-1] == '\r') {
		line = line[:len(line)-1]
	}
	return line
}
