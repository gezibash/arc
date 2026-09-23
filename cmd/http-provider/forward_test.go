package main_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gezibash/arc/delivery/call"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/provider/host"
)

var adapterBinary, originBinary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "arc-http-provider-test")
	if err != nil {
		panic(err)
	}
	adapterBinary = filepath.Join(dir, "http-provider")
	originBinary = filepath.Join(dir, "origin")
	for _, b := range []struct{ out, pkg string }{{adapterBinary, "."}, {originBinary, "./testdata/origin"}} {
		build := exec.Command("go", "build", "-o", b.out, b.pkg)
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			panic(err)
		}
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// serve starts http-provider with a program, as arc serve does.
func serve(t *testing.T, caller call.Caller, program ...string) (*call.Server, *host.Process, keys.Key) {
	t.Helper()
	k := keys.Generate()
	process, err := host.Start(adapterBinary, program, "", nil, quiet)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { process.Stop() })
	return call.NewServer(k, "http", process, 1<<20, caller, quiet), process, k
}

// exchange sends one HTTP request as a call from a caller, and returns the
// status and the body of the response.
func exchange(t *testing.T, server *call.Server, k, from keys.Key, method, path, message string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rumor := call.RequestRumor(from, k.Public, call.Request{Capability: "http", Method: method, Path: path, Body: message}, time.Now())
	reply, err := server.Handle(ctx, rumor)
	if err != nil {
		t.Fatal(err)
	}
	if reply.Err != "" {
		t.Fatalf("the call failed: %s", reply.Err)
	}
	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
	}
	if err := json.Unmarshal([]byte(reply.Body), &out); err != nil {
		t.Fatalf("the reply is not a response: %q", reply.Body)
	}
	return out.Status, out.Body
}

// relay asks the origin to call an address through its endpoint.
func relay(address, token string) string {
	query := url.Values{"address": {address}}
	if token != "" {
		query.Set("token", token)
	}
	message, _ := json.Marshal(map[string]string{"query": query.Encode()})
	return string(message)
}

// The server listens late. The call waits for it, reaches it with its
// method, path, query and body, and names the key that sealed it.
func TestACallReachesTheServerAndComesBack(t *testing.T) {
	server, _, k := serve(t, nil, originBinary)
	from := keys.Generate()

	status, body := exchange(t, server, k, from, "POST", "/echo", `{"query":"a=1","body":"hi"}`)
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil || status != 200 {
		t.Fatalf("status %d, body %q", status, body)
	}
	if got["method"] != "POST" || got["path"] != "/echo" || got["query"] != "a=1" || got["body"] != "hi" {
		t.Errorf("the server got %v", got)
	}
	if got["caller"] != from.Public.Hex() {
		t.Errorf("the server got the caller %q, want %s", got["caller"], from.Public.Hex())
	}
}

// Standard output carries the protocol. The origin writes a line to its own
// standard output, and no such line reaches the protocol.
func TestTheServerNeverWritesToTheProtocol(t *testing.T) {
	adapter := exec.Command(adapterBinary, originBinary)
	in, err := adapter.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := adapter.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Start(); err != nil {
		t.Fatal(err)
	}
	// Close the input first, so http-provider stops its server. A kill alone
	// leaves the server behind.
	t.Cleanup(func() {
		in.Close()
		ended := make(chan struct{})
		go func() { adapter.Wait(); close(ended) }()
		select {
		case <-ended:
		case <-time.After(10 * time.Second):
			adapter.Process.Kill()
			<-ended
		}
	})

	request, _ := json.Marshal(map[string]any{
		"op": "request", "request_id": "r1", "from": strings.Repeat("a", 64),
		"message": "", "meta": map[string]any{"method": "GET", "path": "/echo"},
	})
	in.Write(append(request, '\n'))

	lines := make(chan string)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()

	closed := false
	for {
		select {
		case line, open := <-lines:
			if !open {
				return
			}
			if strings.Contains(line, "the origin writes") {
				t.Fatalf("the origin wrote to the protocol: %q", line)
			}
			var answer map[string]any
			if err := json.Unmarshal([]byte(line), &answer); err != nil {
				t.Fatalf("a line of the protocol is not JSON: %q", line)
			}
			if answer["request_id"] == "r1" && !closed {
				// The answer came. Close the input, and read to the end.
				in.Close()
				closed = true
			}
		case <-time.After(15 * time.Second):
			t.Fatal("http-provider did not answer, or did not end")
		}
	}
}

// The origin calls through its endpoint. A reply, a refusal, and a call that
// could not be made come back as 200, 502, and 503.
func TestTheServerCallsAsTheCitizen(t *testing.T) {
	seen := make(chan call.Outbound, 3)
	caller := func(_ context.Context, out call.Outbound) (call.Reply, error) {
		seen <- out
		switch out.Address {
		case "sqlite+arc://k/main":
			return call.Reply{Body: "rows"}, nil
		case "sqlite+arc://k/refuse":
			return call.Reply{Err: "unauthorized"}, nil
		}
		return call.Reply{}, errors.New("not_installed: install it first")
	}
	server, _, k := serve(t, caller, originBinary)
	from := keys.Generate()

	cases := []struct{ address, want string }{
		{"sqlite+arc://k/main", `200 {"reply":"rows"}`},
		{"sqlite+arc://k/refuse", `502 {"refused":"unauthorized"}`},
		{"sqlite+arc://k/gone", `503 {"error":"not_installed: install it first"}`},
	}
	for _, c := range cases {
		if _, body := exchange(t, server, k, from, "GET", "/relay", relay(c.address, "")); body != c.want {
			t.Errorf("%s gave %q, want %q", c.address, body, c.want)
		}
		if out := <-seen; out.Address != c.address || out.Body != "q" {
			t.Errorf("the caller got %+v", out)
		}
	}
}

// Another program on this machine does not know the token. Its call gets
// 401, and no call goes out.
func TestTheEndpointRefusesAWrongToken(t *testing.T) {
	var calls atomic.Int32
	caller := func(context.Context, call.Outbound) (call.Reply, error) {
		calls.Add(1)
		return call.Reply{Body: "rows"}, nil
	}
	server, _, k := serve(t, caller, originBinary)

	_, body := exchange(t, server, k, keys.Generate(), "GET", "/relay", relay("sqlite+arc://k/main", "wrong"))
	if !strings.HasPrefix(body, "401 ") {
		t.Errorf("a wrong token gave %q, want 401", body)
	}
	// The exchange ended after the endpoint answered, so a call would have
	// run by now.
	if n := calls.Load(); n != 0 {
		t.Errorf("a call with a wrong token went out %d times", n)
	}
}

// The server ends by itself. http-provider ends too, so arc serve knows
// that the provider stopped.
func TestAServerThatEndsStopsTheProvider(t *testing.T) {
	server, _, k := serve(t, nil, originBinary)
	// The request ends the origin. Its answer is a 502, or no answer when
	// http-provider stops first.
	rumor := call.RequestRumor(keys.Generate(), k.Public, call.Request{Capability: "http", Method: "GET", Path: "/exit"}, time.Now())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := server.Handle(ctx, rumor); err != nil {
		t.Fatal(err)
	}
	select {
	case <-server.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("http-provider runs on after its server ended")
	}
}

func TestAServerThatEndsBeforeItListensFails(t *testing.T) {
	server, _, _ := serve(t, nil, "/bin/sh", "-c", "exit 3")
	select {
	case <-server.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("http-provider waits for a server that ended")
	}
}

// arc serve stops the provider. The server stops too: no process stays
// behind on its port.
func TestAStopEndsTheServer(t *testing.T) {
	server, process, k := serve(t, nil, originBinary)
	_, body := exchange(t, server, k, keys.Generate(), "GET", "/echo", "")
	var got map[string]string
	if err := json.Unmarshal([]byte(body), &got); err != nil || got["port"] == "" {
		t.Fatalf("the origin named no port: %q", body)
	}
	address := net.JoinHostPort("127.0.0.1", got["port"])

	if err := process.Stop(); err != nil {
		t.Fatalf("http-provider did not end cleanly: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			return
		}
		conn.Close()
		if time.Now().After(deadline) {
			t.Fatalf("the server still listens on %s after the stop", address)
		}
	}
}
