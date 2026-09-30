// Package server serves an HTTP server of any language over ARC, as
// docs/http/SPEC.md defines. It starts the server, and forwards each call to
// it. The server calls other capabilities through a local endpoint.
//
//	arc-http <program> [args...]
//
// The server gets three variables:
//
//	PORT            the port on 127.0.0.1 where the server listens
//	ARC_CALL_URL    where the server posts a call to another capability
//	ARC_CALL_TOKEN  the bearer token of each such call
package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	httpadapter "github.com/gezibash/arc/adapters/http"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/strictjson"
)

const (
	// listenTime bounds how long the server takes to listen on PORT.
	listenTime = 30 * time.Second
	// answerTime bounds one exchange with the server. arc serve gives one
	// request 60 seconds, so the server gets less.
	answerTime = 50 * time.Second
	// stopGrace is how long the server has to end after SIGTERM. arc serve
	// gives this program 5 seconds, so the server gets less.
	stopGrace = 3 * time.Second
	// maxCall caps the request of one call of the server.
	maxCall = 4 * 1024 * 1024
)

// Run hosts an HTTP program and serves its interface through core ARC.
// The caller supplies protocol streams, diagnostics and cancellation.
func Run(ctx context.Context, command []string, opts provider.Options) error {
	if len(command) == 0 {
		return errors.New("name the HTTP program to run")
	}
	if opts.Log == nil {
		opts.Log = io.Discard
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	token, err := newToken()
	if err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	origin := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
	a := &adapter{token: token, web: httpadapter.New(forward(origin, opts.Log))}
	calls, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	endpoint := &http.Server{Handler: a.calls(), ReadHeaderTimeout: 10 * time.Second}
	go endpoint.Serve(calls)
	defer endpoint.Close()
	s, err := start(command, []string{
		"PORT=" + port,
		"ARC_CALL_URL=http://" + calls.Addr().String() + "/call",
		"ARC_CALL_TOKEN=" + token,
	}, opts.Log)
	if err != nil {
		return err
	}
	defer s.stop()
	if err := s.listening(ctx, origin.Host); err != nil {
		return err
	}
	// A child exit ends the interaction. Returning the cause lets the entry
	// point report it without terminating an embedding process.
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-s.done:
			cancel(fmt.Errorf("the HTTP program stopped: %v", s.err))
		case <-ctx.Done():
		case <-watchDone:
		}
	}()
	err = provider.Run(ctx, a, opts)
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return err
}

// adapter answers each call with the server, and gives the server a caller.
type adapter struct {
	web   *httpadapter.Adapter
	token string

	mu            sync.Mutex
	caller        provider.Caller
	sessionCaller provider.SessionCaller
}

func (a *adapter) HandleRequest(ctx context.Context, r provider.Request) (string, error) {
	return a.web.HandleRequest(ctx, r)
}

func (a *adapter) HandleSession(ctx context.Context, req provider.Request, stream *session.Stream) error {
	return a.web.HandleSession(context.WithValue(ctx, sessionHTTPKey{}, true), req, stream)
}
func (a *adapter) SetSessionCaller(caller provider.SessionCaller) {
	a.mu.Lock()
	a.sessionCaller = caller
	a.mu.Unlock()
}

func (a *adapter) SetCaller(caller provider.Caller) {
	a.mu.Lock()
	a.caller = caller
	a.mu.Unlock()
}

// calls is the endpoint of the server for its calls. It listens on
// 127.0.0.1, and takes only a request with the token, so no other program on
// this machine calls as this citizen.
func (a *adapter) calls() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/call" && r.URL.Path != "/session" {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			answer(w, http.StatusMethodNotAllowed, "error", "a call is a POST")
			return
		}
		given, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(given), []byte(a.token)) != 1 {
			answer(w, http.StatusUnauthorized, "error", "the token is not ARC_CALL_TOKEN")
			return
		}
		if r.URL.Path == "/session" {
			a.sessionCall(w, r)
			return
		}
		var in struct {
			Address string  `json:"address"`
			Body    *string `json:"body"`
		}
		if err := strictjson.Decode(io.LimitReader(r.Body, maxCall), &in); err != nil || in.Address == "" || in.Body == nil {
			answer(w, http.StatusBadRequest, "error", `a call is {"address": "<scheme>+arc://<provider>/<path>", "body": "..."}`)
			return
		}

		a.mu.Lock()
		caller := a.caller
		a.mu.Unlock()
		if caller == nil {
			answer(w, http.StatusServiceUnavailable, "error", "the provider does not serve yet")
			return
		}

		reply, err := caller.Call(r.Context(), in.Address, *in.Body)
		var failed *provider.CallError
		switch {
		case errors.As(err, &failed) && failed.Refused:
			answer(w, http.StatusBadGateway, "refused", failed.Reason)
		case errors.As(err, &failed):
			answer(w, http.StatusServiceUnavailable, "error", failed.Reason)
		case err != nil:
			answer(w, http.StatusServiceUnavailable, "error", err.Error())
		default:
			answer(w, http.StatusOK, "reply", reply)
		}
	})
}

func answer(w http.ResponseWriter, status int, field, value string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{field: value})
}

// forward sends each request to the server. A server that does not answer
// in answerTime, or that cannot be reached, gives 502.
func forward(origin *url.URL, logs io.Writer) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(origin) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			fmt.Fprintf(logs, "arc-http: %v\n", err)
			http.Error(w, "the server did not answer", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Value(sessionHTTPKey{}) != nil {
			proxy.ServeHTTP(w, r)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), answerTime)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

// server is the HTTP server that this program runs.
type server struct {
	command *exec.Cmd
	done    chan struct{}
	// err says why the server ended. It is set before done closes.
	err error
}

func start(command []string, env []string, logs io.Writer) (*server, error) {
	c := exec.Command(command[0], command[1:]...)
	c.Env = append(os.Environ(), env...)
	// Standard output carries the ARC protocol, so the server never writes
	// to it.
	c.Stdout, c.Stderr = logs, logs
	if err := c.Start(); err != nil {
		return nil, fmt.Errorf("the server did not start: %w", err)
	}
	s := &server{command: c, done: make(chan struct{})}
	go func() {
		s.err = c.Wait()
		close(s.done)
	}()
	return s, nil
}

// listening waits until the server takes a connection on its port.
func (s *server) listening(ctx context.Context, host string) error {
	deadline := time.Now().Add(listenTime)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.done:
			return fmt.Errorf("the server ended before it listened on %s: %v", host, s.err)
		default:
		}
		if conn, err := net.DialTimeout("tcp", host, time.Second); err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("the server did not listen on %s in %s", host, listenTime)
}

// stop ends the server: SIGTERM, and SIGKILL after stopGrace.
func (s *server) stop() {
	select {
	case <-s.done:
		return
	default:
	}
	s.command.Process.Signal(syscall.SIGTERM)
	select {
	case <-s.done:
	case <-time.After(stopGrace):
		s.command.Process.Kill()
		<-s.done
	}
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// freePort returns a port on 127.0.0.1 that no program uses now.
func freePort() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, err := net.SplitHostPort(l.Addr().String())
	return port, err
}
