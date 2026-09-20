package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gezibash/arc/provider"
)

type echo struct{}

func (echo) HandleRequest(_ context.Context, r provider.Request) (string, error) {
	return r.Message, nil
}

func request(id string, message string, meta map[string]any) string {
	if meta == nil {
		meta = map[string]any{"method": "EXEC"}
	}
	line, _ := json.Marshal(map[string]any{
		"op":         "request",
		"request_id": id,
		"from":       strings.Repeat("a", 64),
		"message":    message,
		"meta":       meta,
		"framed":     true,
	})
	return string(line) + "\n"
}

func run(t *testing.T, handler provider.Handler, input string, opts provider.Options) []map[string]any {
	t.Helper()

	var out bytes.Buffer
	var log bytes.Buffer
	opts.In = strings.NewReader(input)
	opts.Out = &out
	opts.Log = &log

	if err := provider.Run(context.Background(), handler, opts); err != nil {
		t.Fatalf("run: %v", err)
	}

	var answers []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if line == "" {
			continue
		}
		var answer map[string]any
		if err := json.Unmarshal([]byte(line), &answer); err != nil {
			t.Fatalf("the answer is not JSON: %q", line)
		}
		answers = append(answers, answer)
	}
	return answers
}

func TestAnswersOneRequest(t *testing.T) {
	answers := run(t, echo{}, request("r1", "hello", nil), provider.Options{})

	if len(answers) != 1 {
		t.Fatalf("got %d answers, want 1", len(answers))
	}
	if answers[0]["op"] != "reply" || answers[0]["reply"] != "hello" || answers[0]["request_id"] != "r1" {
		t.Errorf("answer = %v", answers[0])
	}
}

func TestTheRequestCarriesItsFields(t *testing.T) {
	var got provider.Request
	handler := provider.HandlerFunc(func(_ context.Context, r provider.Request) (string, error) {
		got = r
		return "", nil
	})

	run(t, handler, request("r1", "body", map[string]any{"method": "EXEC", "path": "/run"}), provider.Options{})

	if got.Method() != "EXEC" || got.Path() != "/run" {
		t.Errorf("meta = %v", got.Meta)
	}
	if got.Message != "body" || got.From != strings.Repeat("a", 64) || !got.Framed {
		t.Errorf("request = %+v", got)
	}
}

func TestASafeErrorReachesTheCaller(t *testing.T) {
	handler := provider.HandlerFunc(func(_ context.Context, _ provider.Request) (string, error) {
		return "", provider.Error("access_denied")
	})

	answers := run(t, handler, request("r1", "x", nil), provider.Options{})
	if answers[0]["op"] != "error" || answers[0]["error"] != "access_denied" {
		t.Errorf("answer = %v", answers[0])
	}
}

func TestAnUnexpectedErrorStaysInside(t *testing.T) {
	handler := provider.HandlerFunc(func(_ context.Context, _ provider.Request) (string, error) {
		return "", errors.New("the database at 10.0.0.5 refused the password hunter2")
	})

	var log bytes.Buffer
	answers := run(t, handler, request("r1", "x", nil), provider.Options{Log: &log})

	if answers[0]["error"] != "internal_error" {
		t.Errorf("the caller saw %v", answers[0]["error"])
	}
}

func TestAPanicFailsOneRequestOnly(t *testing.T) {
	handler := provider.HandlerFunc(func(_ context.Context, r provider.Request) (string, error) {
		if r.Message == "boom" {
			panic("the handler broke")
		}
		return "fine", nil
	})

	answers := run(t, handler, request("r1", "boom", nil)+request("r2", "ok", nil), provider.Options{})
	if len(answers) != 2 {
		t.Fatalf("got %d answers, want 2", len(answers))
	}

	byID := map[string]map[string]any{}
	for _, answer := range answers {
		byID[answer["request_id"].(string)] = answer
	}
	if byID["r1"]["error"] != "internal_error" {
		t.Errorf("the panic gave %v", byID["r1"])
	}
	if byID["r2"]["reply"] != "fine" {
		t.Errorf("the second request gave %v", byID["r2"])
	}
}

func TestRefusesAnEventThatIsNotARequest(t *testing.T) {
	lines := []string{
		`{"op":"stream_open","request_id":"r1","message":"x","meta":{}}`,
		`{"op":"request","request_id":"r1","meta":{}}`,
		`{"op":"request","request_id":true,"message":"x","meta":{}}`,
		`{"op":"request","request_id":"r1","message":"x"}`,
		`not json`,
	}

	for _, line := range lines {
		answers := run(t, echo{}, line+"\n", provider.Options{})
		if len(answers) != 1 || answers[0]["error"] != "invalid_request" {
			t.Errorf("%s gave %v", line, answers)
		}
	}
}

func TestDropsALineOverTheCapAndKeepsServing(t *testing.T) {
	long := request("r1", strings.Repeat("x", 4096), nil)
	input := long + request("r2", "after", nil)

	answers := run(t, echo{}, input, provider.Options{MaxLineBytes: 1024})
	if len(answers) != 2 {
		t.Fatalf("got %d answers, want 2", len(answers))
	}
	if answers[0]["error"] != "request_too_large" || answers[0]["request_id"] != nil {
		t.Errorf("the long line gave %v", answers[0])
	}
	if answers[1]["reply"] != "after" {
		t.Errorf("the next line gave %v", answers[1])
	}
}

func TestOneSlowRequestDoesNotHoldUpAnother(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once

	handler := provider.HandlerFunc(func(_ context.Context, r provider.Request) (string, error) {
		if r.Message == "slow" {
			<-release
			return "slow", nil
		}
		once.Do(func() { close(release) })
		return "fast", nil
	})

	done := make(chan []map[string]any, 1)
	go func() {
		done <- run(t, handler, request("slow", "slow", nil)+request("fast", "fast", nil), provider.Options{})
	}()

	select {
	case answers := <-done:
		if len(answers) != 2 {
			t.Fatalf("got %d answers, want 2", len(answers))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the fast request waited for the slow one")
	}
}

// Two answers must never share a line.
func TestAnswersNeverInterleave(t *testing.T) {
	handler := provider.HandlerFunc(func(_ context.Context, r provider.Request) (string, error) {
		return strings.Repeat(r.Message, 2000), nil
	})

	var input strings.Builder
	for index := 0; index < 50; index++ {
		input.WriteString(request(fmt.Sprintf("r%d", index), fmt.Sprintf("%d", index%10), nil))
	}

	answers := run(t, handler, input.String(), provider.Options{})
	if len(answers) != 50 {
		t.Fatalf("got %d answers, want 50", len(answers))
	}
	for _, answer := range answers {
		reply, _ := answer["reply"].(string)
		if len(reply) != 2000 {
			t.Fatalf("an answer is %d characters", len(reply))
		}
	}
}
