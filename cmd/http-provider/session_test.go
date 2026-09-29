package main

import (
	"context"
	"github.com/gezibash/arc/core/provider"
	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/internal/testsession"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

type nestedCaller func(context.Context, string, string, session.Mode) (*session.Stream, error)

func (f nestedCaller) OpenSession(ctx context.Context, address, body string, mode session.Mode) (*session.Stream, error) {
	return f(ctx, address, body, mode)
}
func TestHTTPNestedSessionStreamsAndPreservesFinalError(t *testing.T) {
	a := &adapter{token: "test"}
	a.SetSessionCaller(nestedCaller(func(ctx context.Context, address, body string, mode session.Mode) (*session.Stream, error) {
		if address != "sqlite+arc://provider/main" || body != "query" || mode != session.ServerStream {
			t.Errorf("nested arguments: %s %s %s", address, body, mode)
		}
		return testsession.Start(t, mode, func(_ context.Context, s *session.Stream) error {
			if _, err := io.WriteString(s, "first"); err != nil {
				return err
			}
			return provider.Error("refused")
		}), nil
	}))
	server := httptest.NewServer(a.calls())
	defer server.Close()
	target := server.URL + "/session?" + url.Values{"address": {"sqlite+arc://provider/main"}, "body": {"query"}}.Encode()
	request, _ := http.NewRequest("POST", target, strings.NewReader(""))
	request.Header.Set("Authorization", "Bearer test")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil || string(data) != "first" || !strings.Contains(response.Trailer.Get("Arc-Session-Error"), "refused") {
		t.Fatalf("body=%q trailers=%v err=%v", data, response.Trailer, err)
	}
	request, _ = http.NewRequest("POST", target, nil)
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != 401 {
		t.Fatalf("unguarded nested session: %d", response.StatusCode)
	}
}
