package provider_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gezibash/arc/provider"
)

var caller = strings.Repeat("c", 64)

// callHTTP sends one call to an HTTP handler through the adapter.
func callHTTP(handler http.Handler, method, path, message string) (string, error) {
	return provider.HTTP(handler).HandleRequest(context.Background(), provider.Request{
		Op: "request", From: caller, Message: message,
		Meta: map[string]any{"method": method, "path": path},
	})
}

// response reads a reply of the adapter.
func response(t *testing.T, reply string) (status int, headers http.Header, body string, binary []byte) {
	t.Helper()
	var out struct {
		Status     int                 `json:"status"`
		Headers    map[string][]string `json:"headers"`
		Body       string              `json:"body"`
		BodyBase64 string              `json:"body_base64"`
	}
	if err := json.Unmarshal([]byte(reply), &out); err != nil {
		t.Fatalf("the reply is not a response: %q", reply)
	}
	if out.BodyBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(out.BodyBase64)
		if err != nil {
			t.Fatal(err)
		}
		binary = decoded
	}
	return out.Status, http.Header(out.Headers), out.Body, binary
}

// One exchange, worked by hand: POST /notes?tag=a with a JSON body. The
// handler answers 201 with a Location field.
func TestAnHTTPExchangeCrossesTheAdapter(t *testing.T) {
	var got struct {
		method, path, tag, contentType, connection, body string
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.method, got.path, got.tag = r.Method, r.URL.Path, r.URL.Query().Get("tag")
		got.contentType, got.connection, got.body = r.Header.Get("Content-Type"), r.Header.Get("Connection"), string(body)
		w.Header().Set("Location", "/notes/7")
		w.Header().Set("Connection", "close")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, `{"id":7}`)
	})

	message := `{"query":"tag=a","headers":{"Content-Type":["application/json"],"Connection":["close"]},"body":"{\"text\":\"hi\"}"}`
	reply, err := callHTTP(handler, "POST", "/notes", message)
	if err != nil {
		t.Fatal(err)
	}

	if got.method != "POST" || got.path != "/notes" || got.tag != "a" || got.contentType != "application/json" || got.body != `{"text":"hi"}` {
		t.Errorf("the handler got %+v", got)
	}
	if got.connection != "" {
		t.Errorf("the handler got the hop-by-hop field Connection: %q", got.connection)
	}
	status, headers, body, _ := response(t, reply)
	if status != http.StatusCreated || headers.Get("Location") != "/notes/7" || body != `{"id":7}` {
		t.Errorf("reply = %s", reply)
	}
	if headers.Get("Connection") != "" {
		t.Errorf("the reply carries the hop-by-hop field Connection: %s", reply)
	}
}

// An empty message is a request with no query, no fields, and no body.
func TestAnEmptyMessageIsARequestWithNoBody(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		io.WriteString(w, r.Method+" "+r.URL.String()+" "+string(body))
	})
	reply, err := callHTTP(handler, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if status, _, body, _ := response(t, reply); status != http.StatusOK || body != "GET http://arc/ " {
		t.Errorf("reply = %s", reply)
	}
}

// A caller cannot name another caller: the adapter drops the Arc-Caller
// field of the message, and sets the key that signed the call.
func TestTheCallerFieldCannotBeForged(t *testing.T) {
	var seen []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Values(provider.CallerHeader)
	})
	forged := `{"headers":{"arc-caller":["` + strings.Repeat("f", 64) + `"]}}`
	if _, err := callHTTP(handler, "GET", "/", forged); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != caller {
		t.Errorf("the handler saw the callers %q, want only %s", seen, caller)
	}
}

// A body that is not UTF-8 travels as base64 both ways, byte for byte.
func TestBinaryBodiesTravelAsBase64(t *testing.T) {
	sent := []byte{0xff, 0x00, 0xfe}
	var got []byte
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.Write([]byte{0x89, 'P', 'N', 'G'})
	})
	message := `{"body_base64":"` + base64.StdEncoding.EncodeToString(sent) + `"}`
	reply, err := callHTTP(handler, "PUT", "/image", message)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sent) {
		t.Errorf("the handler got %x, want %x", got, sent)
	}
	if _, _, body, binary := response(t, reply); body != "" || string(binary) != "\x89PNG" {
		t.Errorf("reply = %s", reply)
	}
}

func TestAMessageThatIsNotARequestIsRefused(t *testing.T) {
	ran := false
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { ran = true })
	for _, message := range []string{
		`not json`,
		`{"verb":"GET"}`,
		`{"body":"a","body_base64":"Yg=="}`,
		`{"query":"a=%zz"}`,
	} {
		if _, err := callHTTP(handler, "GET", "/", message); !errors.Is(err, provider.ErrInvalidRequest) {
			t.Errorf("%s gave %v, want invalid_request", message, err)
		}
	}
	if _, err := callHTTP(handler, "BAD METHOD", "/", ""); !errors.Is(err, provider.ErrInvalidRequest) {
		t.Errorf("a method with a space gave %v, want invalid_request", err)
	}
	if ran {
		t.Error("the handler ran for a message that is not a request")
	}
}

// A response over the limit fails whole. The caller never gets part of it.
func TestAResponseOverTheLimitFails(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(strings.Repeat("a", provider.MaxHTTPBody)))
		w.Write([]byte("b"))
	})
	reply, err := callHTTP(handler, "GET", "/", "")
	if !errors.Is(err, provider.ErrResponseTooLarge) || reply != "" {
		t.Errorf("got %d bytes and %v, want response_too_large", len(reply), err)
	}
}
