// Command http-provider serves an HTTP server of any language over ARC, as
// docs/http/SPEC.md defines. It starts the server, and forwards each call to
// it. The server calls other capabilities through a local endpoint.
//
//	http-provider <program> [args...]
//
// The server gets three variables:
//
//	PORT            the port on 127.0.0.1 where the server listens
//	ARC_CALL_URL    where the server posts a call to another capability
//	ARC_CALL_TOKEN  the bearer token of each such call
package main

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
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gezibash/arc/provider"
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

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: http-provider <program> [args...]")
		os.Exit(2)
	}
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "http-provider: %v\n", err)
		os.Exit(1)
	}
}

func run(command []string) error {
	token, err := newToken()
	if err != nil {
		return err
	}
	port, err := freePort()
	if err != nil {
		return err
	}
	origin := &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", port)}
	a := &adapter{token: token, web: provider.HTTP(forward(origin))}

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
	})
	if err != nil {
		return err
	}
	if err := s.listening(origin.Host); err != nil {
		s.stop()
		return err
	}

	// The server ends by itself: so does this program, and arc serve says
	// that the provider stopped. A stop that this program asked for is not
	// an error.
	var stopping atomic.Bool
	go func() {
		<-s.done
		if !stopping.Load() {
			fmt.Fprintf(os.Stderr, "http-provider: the server stopped: %v\n", s.err)
			os.Exit(1)
		}
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-signals
		stopping.Store(true)
		s.stop()
		os.Exit(1)
	}()

	err = provider.Run(context.Background(), a, provider.Options{})
	stopping.Store(true)
	s.stop()
	return err
}

// adapter answers each call with the server, and gives the server a caller.
type adapter struct {
	web   provider.Handler
	token string

	mu     sync.Mutex
	caller provider.Caller
}

func (a *adapter) HandleRequest(ctx context.Context, r provider.Request) (string, error) {
	return a.web.HandleRequest(ctx, r)
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
		if r.URL.Path != "/call" {
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
		var in struct {
			Address string  `json:"address"`
			Body    *string `json:"body"`
		}
		decoder := json.NewDecoder(io.LimitReader(r.Body, maxCall))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil || in.Address == "" || in.Body == nil {
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
func forward(origin *url.URL) http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) { r.SetURL(origin) },
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			fmt.Fprintf(os.Stderr, "http-provider: %v\n", err)
			http.Error(w, "the server did not answer", http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func start(command []string, env []string) (*server, error) {
	c := exec.Command(command[0], command[1:]...)
	c.Env = append(os.Environ(), env...)
	// Standard output carries the ARC protocol, so the server never writes
	// to it.
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
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
func (s *server) listening(host string) error {
	deadline := time.Now().Add(listenTime)
	for time.Now().Before(deadline) {
		select {
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
