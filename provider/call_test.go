package provider_test

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/provider"
)

// calling calls the address that a request names, and answers with the
// reply. It answers "echo" with "echo", and makes no call for it.
type calling struct {
	caller provider.Caller
}

func (c *calling) SetCaller(caller provider.Caller) { c.caller = caller }

func (c *calling) HandleRequest(ctx context.Context, r provider.Request) (string, error) {
	if r.Message == "echo" {
		return "echo", nil
	}
	reply, err := c.caller.Call(ctx, r.Message, "the body")
	var failed *provider.CallError
	switch {
	case errors.As(err, &failed) && failed.Refused:
		return "", provider.Error("refused:" + failed.Reason)
	case errors.As(err, &failed):
		return "", provider.Error("failed:" + failed.Reason)
	case err != nil:
		return "", provider.Error("other:" + err.Error())
	}
	return reply, nil
}

// pipes runs a provider over pipes, so that a test can answer its calls.
type pipes struct {
	in   *io.PipeWriter
	out  *bufio.Reader
	done chan error
}

func startPipes(t *testing.T, handler provider.Handler) *pipes {
	t.Helper()
	inRead, inWrite := io.Pipe()
	outRead, outWrite := io.Pipe()
	p := &pipes{in: inWrite, out: bufio.NewReader(outRead), done: make(chan error, 1)}
	go func() {
		err := provider.Run(context.Background(), handler, provider.Options{In: inRead, Out: outWrite, Log: io.Discard})
		outWrite.Close()
		p.done <- err
	}()
	t.Cleanup(func() { inWrite.Close() })
	return p
}

func (p *pipes) send(t *testing.T, line map[string]any) {
	t.Helper()
	body, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.in.Write(append(body, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (p *pipes) request(t *testing.T, id, message string) {
	t.Helper()
	p.send(t, map[string]any{
		"op": "request", "request_id": id, "from": strings.Repeat("a", 64),
		"message": message, "meta": map[string]any{"method": "GET"},
	})
}

// read returns the next line of the provider, or nil at the end of its
// output.
func (p *pipes) read(t *testing.T) map[string]any {
	t.Helper()
	type got struct {
		line string
		err  error
	}
	next := make(chan got, 1)
	go func() {
		line, err := p.out.ReadString('\n')
		next <- got{line, err}
	}()
	select {
	case g := <-next:
		if errors.Is(g.err, io.EOF) && g.line == "" {
			return nil
		}
		if g.err != nil {
			t.Fatal(g.err)
		}
		var line map[string]any
		if err := json.Unmarshal([]byte(g.line), &line); err != nil {
			t.Fatalf("the provider wrote a line that is not JSON: %q", g.line)
		}
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("the provider wrote nothing for 5 seconds")
		return nil
	}
}

func TestACallGoesOutAndItsReplyComesBack(t *testing.T) {
	p := startPipes(t, &calling{})
	p.request(t, "r1", "sqlite+arc://k/main")

	call := p.read(t)
	id, _ := call["call_id"].(string)
	if call["op"] != "call" || call["address"] != "sqlite+arc://k/main" || call["body"] != "the body" || id == "" {
		t.Fatalf("call line = %v", call)
	}
	p.send(t, map[string]any{"op": "result", "call_id": id, "reply": "rows"})

	if answer := p.read(t); answer["op"] != "reply" || answer["request_id"] != "r1" || answer["reply"] != "rows" {
		t.Errorf("answer = %v", answer)
	}
}

// Two calls wait at once, and their results come in the other order. Each
// result reaches its own call.
func TestResultsReachTheirOwnCalls(t *testing.T) {
	p := startPipes(t, &calling{})
	p.request(t, "r1", "a+arc://k/one")
	p.request(t, "r2", "b+arc://k/two")

	ids := map[string]string{}
	for range 2 {
		call := p.read(t)
		address, _ := call["address"].(string)
		ids[address], _ = call["call_id"].(string)
	}
	if ids["a+arc://k/one"] == "" || ids["a+arc://k/one"] == ids["b+arc://k/two"] {
		t.Fatalf("the calls have the ids %v", ids)
	}
	p.send(t, map[string]any{"op": "result", "call_id": ids["b+arc://k/two"], "reply": "second"})
	p.send(t, map[string]any{"op": "result", "call_id": ids["a+arc://k/one"], "reply": "first"})

	replies := map[any]any{}
	for range 2 {
		answer := p.read(t)
		replies[answer["request_id"]] = answer["reply"]
	}
	if replies["r1"] != "first" || replies["r2"] != "second" {
		t.Errorf("replies = %v", replies)
	}
}

// A provider that got the call refused it, or ARC could not make it. The
// handler can tell the two apart.
func TestARefusalIsNotAFailure(t *testing.T) {
	p := startPipes(t, &calling{})
	cases := []struct {
		result map[string]any
		want   string
	}{
		{map[string]any{"refused": "invalid_request"}, "refused:invalid_request"},
		{map[string]any{"error": "not_installed"}, "failed:not_installed"},
	}
	for _, c := range cases {
		p.request(t, "r", "x+arc://k/")
		call := p.read(t)
		c.result["op"], c.result["call_id"] = "result", call["call_id"]
		p.send(t, c.result)
		if answer := p.read(t); answer["op"] != "error" || answer["error"] != c.want {
			t.Errorf("answer = %v, want the error %q", answer, c.want)
		}
	}
}

// The input ends while a call waits. The call fails, the request gets its
// answer, and Run returns.
func TestACallFailsWhenTheInputEnds(t *testing.T) {
	p := startPipes(t, &calling{})
	p.request(t, "r1", "x+arc://k/")
	if call := p.read(t); call["op"] != "call" {
		t.Fatalf("line = %v, want a call", call)
	}
	p.in.Close()

	answer := p.read(t)
	message, _ := answer["error"].(string)
	if answer["request_id"] != "r1" || !strings.HasPrefix(message, "failed:") {
		t.Errorf("answer = %v, want a failed call", answer)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the input ended")
	}
}

// A result that no call waits for gets no answer: ARC would read an answer
// as the reply to a request.
func TestAResultForNoCallGetsNoAnswer(t *testing.T) {
	p := startPipes(t, &calling{})
	p.send(t, map[string]any{"op": "result", "call_id": "99", "reply": "stray"})
	p.request(t, "r1", "echo")

	if answer := p.read(t); answer["request_id"] != "r1" || answer["reply"] != "echo" {
		t.Fatalf("answer = %v, want the reply to r1", answer)
	}
	p.in.Close()
	if extra := p.read(t); extra != nil {
		t.Errorf("the provider answered the stray result: %v", extra)
	}
}
