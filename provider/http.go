package provider

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"
)

// CallerHeader is the header field that carries the public key of the
// caller, in lower-case hex, to an HTTP handler. The adapter sets it. It
// drops a field of this name that the caller sent.
const CallerHeader = "Arc-Caller"

// MaxHTTPBody caps the body of one HTTP response. A larger response fails
// with response_too_large. The adapter never cuts a response short.
const MaxHTTPBody = 1024 * 1024

// ErrResponseTooLarge answers a call whose HTTP response is over MaxHTTPBody.
const ErrResponseTooLarge = Error("response_too_large")

// httpRequest is the message of a call to an HTTP capability. The method and
// the path of the call are the method and the path of the HTTP request. See
// docs/http/SPEC.md.
type httpRequest struct {
	Query      string              `json:"query,omitempty"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       string              `json:"body,omitempty"`
	BodyBase64 string              `json:"body_base64,omitempty"`
}

// httpResponse is the reply of an HTTP capability.
type httpResponse struct {
	Status     int                 `json:"status"`
	Headers    map[string][]string `json:"headers,omitempty"`
	Body       string              `json:"body,omitempty"`
	BodyBase64 string              `json:"body_base64,omitempty"`
}

// HTTP serves an http.Handler as a provider. Each call becomes one HTTP
// request to the handler, and the response becomes the reply, as
// docs/http/SPEC.md defines. The handler runs in this process: no port
// opens.
func HTTP(handler http.Handler) Handler {
	return HandlerFunc(func(ctx context.Context, r Request) (string, error) {
		request, err := httpRequestOf(ctx, r)
		if err != nil {
			return "", ErrInvalidRequest
		}

		response := &recorder{header: http.Header{}}
		handler.ServeHTTP(response, request)
		if response.over {
			return "", ErrResponseTooLarge
		}
		return response.reply()
	})
}

// httpRequestOf turns a call into an HTTP request.
func httpRequestOf(ctx context.Context, r Request) (*http.Request, error) {
	var in httpRequest
	if strings.TrimSpace(r.Message) != "" {
		decoder := json.NewDecoder(strings.NewReader(r.Message))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil {
			return nil, err
		}
	}
	if in.Body != "" && in.BodyBase64 != "" {
		return nil, errors.New("provider: a request has body or body_base64, not both")
	}
	body := []byte(in.Body)
	if in.BodyBase64 != "" {
		decoded, err := base64.StdEncoding.DecodeString(in.BodyBase64)
		if err != nil {
			return nil, err
		}
		body = decoded
	}
	if strings.Contains(in.Query, "#") {
		return nil, errors.New("provider: a query has no fragment")
	}
	if _, err := url.ParseQuery(in.Query); err != nil {
		return nil, err
	}

	method, path := r.Method(), r.Path()
	if method == "" {
		method = http.MethodGet
	}
	if path == "" {
		path = "/"
	}
	target := url.URL{Scheme: "http", Host: "arc", Path: path, RawQuery: in.Query}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	for name, values := range in.Headers {
		if hopByHop(name) {
			continue
		}
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	// Set replaces each value of the field that the caller sent.
	request.Header.Set(CallerHeader, r.From)
	return request, nil
}

// hopByHop says whether a header field belongs to one connection, as RFC
// 9110 section 7.6.1 lists. No connection exists here, so the adapter drops
// these fields both ways. It drops Content-Length too, because the body sets
// it.
func hopByHop(name string) bool {
	switch http.CanonicalHeaderKey(name) {
	case "Connection", "Keep-Alive", "Proxy-Connection", "Te", "Trailer", "Transfer-Encoding", "Upgrade", "Content-Length":
		return true
	}
	return false
}

// recorder keeps one response. It keeps the header fields as they were when
// the handler sent the status, as net/http does.
type recorder struct {
	header http.Header
	sent   http.Header
	status int
	body   bytes.Buffer
	over   bool
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(status int) {
	// A 1xx status is not the final one.
	if r.status != 0 || status < 200 {
		return
	}
	r.status = status
	r.sent = r.header.Clone()
}

func (r *recorder) Write(p []byte) (int, error) {
	r.WriteHeader(http.StatusOK)
	if r.body.Len()+len(p) > MaxHTTPBody {
		r.over = true
		return 0, ErrResponseTooLarge
	}
	return r.body.Write(p)
}

// reply writes the response as the reply of the call.
func (r *recorder) reply() (string, error) {
	r.WriteHeader(http.StatusOK)
	out := httpResponse{Status: r.status}
	for name, values := range r.sent {
		if hopByHop(name) {
			continue
		}
		if out.Headers == nil {
			out.Headers = map[string][]string{}
		}
		out.Headers[name] = values
	}
	if body := r.body.Bytes(); utf8.Valid(body) {
		out.Body = string(body)
	} else {
		out.BodyBase64 = base64.StdEncoding.EncodeToString(body)
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}
