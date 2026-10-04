package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	execadapter "github.com/gezibash/arc/sdk/execadapter"
	"github.com/gezibash/arc/sdk/provider"
)

// keptServer is a test server that stops its kept processes at the end.
func keptServer(t *testing.T) *server {
	s := testServer(t)
	t.Cleanup(s.stopKept)
	return s
}

// session reads the output records of one session.
type session struct {
	t      *testing.T
	stream *provider.Stream
	reader *execadapter.Reader
	out    strings.Builder
	exit   int
}

func open(t *testing.T, s *server, from, body string) *session {
	stream := execStream(t, s, from, body, provider.Duplex)
	return &session{t: t, stream: stream, reader: execadapter.NewReader(stream), exit: -1}
}

// until reads output until it holds text.
func (c *session) until(text string) {
	c.t.Helper()
	for !strings.Contains(c.out.String(), text) {
		record, err := c.reader.Next()
		if err != nil {
			c.t.Fatalf("waiting for %q after %q: %v", text, c.out.String(), err)
		}
		c.out.Write(record.Data)
	}
}

// finish reads output until the session ends, and returns its final error.
func (c *session) finish() error {
	c.t.Helper()
	for {
		record, err := c.reader.Next()
		if errors.Is(err, io.EOF) {
			return c.stream.Wait()
		}
		if err != nil {
			return err
		}
		c.out.Write(record.Data)
		if record.Type == "exit" {
			c.exit = record.Exit
		}
	}
}

func (c *session) send(text string) {
	c.t.Helper()
	if err := json.NewEncoder(c.stream).Encode(execadapter.Record{Type: "stdin", Data: []byte(text)}); err != nil {
		c.t.Fatal(err)
	}
}

// buffered returns the output in the buffer of a kept process.
func buffered(k *keptProcess) string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out strings.Builder
	chunks, _ := k.output.since(0)
	for _, chunk := range chunks {
		out.Write(chunk.data)
	}
	return out.String()
}

func eventually(t *testing.T, what string, check func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if check() {
			return
		}
	}
	t.Fatalf("%s did not happen", what)
}

func TestKeptProcessOutlivesItsSession(t *testing.T) {
	s := keptServer(t)
	first := open(t, s, caller, `{"script":"echo one; read line; echo \"two $line\"; read line; echo \"three $line\"; exit 5","keep":"alfred"}`)
	first.until("one")
	first.send("a\n")
	first.until("two a")
	if err := first.stream.Close(); err != nil {
		t.Fatal(err)
	}
	if err := first.finish(); err == nil {
		t.Fatal("a closed session reported success")
	}
	k := s.find("alfred")
	if k == nil || k.done() || !alive(k.process.Process.Pid) {
		t.Fatal("the process stopped when its session ended")
	}

	second := open(t, s, caller, `{"attach":"alfred"}`)
	second.until("two a")
	second.send("b\n")
	if err := second.finish(); err != nil {
		t.Fatal(err)
	}
	if second.out.String() != "one\ntwo a\nthree b\n" || second.exit != 5 {
		t.Fatalf("attach: %q exit %d", second.out.String(), second.exit)
	}
	if s.find("alfred") != nil {
		t.Fatal("the name stays after a session showed the exit status")
	}
}

func TestAttachAfterExitShowsTheExitStatus(t *testing.T) {
	s := keptServer(t)
	first := open(t, s, caller, `{"script":"echo hi; read line; echo bye; exit 3","keep":"gone"}`)
	first.until("hi")
	_ = first.stream.Close()
	_ = first.finish()
	k := s.find("gone")
	if k == nil {
		t.Fatal("the process has no name")
	}
	// The process reads the input that no session is attached to.
	if err := k.write([]byte("x\n")); err != nil {
		t.Fatal(err)
	}
	<-k.ended

	second := open(t, s, caller, `{"attach":"gone"}`)
	if err := second.finish(); err != nil {
		t.Fatal(err)
	}
	if second.out.String() != "hi\nbye\n" || second.exit != 3 {
		t.Fatalf("attach after exit: %q exit %d", second.out.String(), second.exit)
	}
	third := open(t, s, caller, `{"attach":"gone"}`)
	if err := third.finish(); err == nil || err.Error() != "not_found" {
		t.Fatalf("attach after the exit was shown: %v", err)
	}
}

// The buffer keeps the newest output_bytes bytes, and drops the older bytes.
func TestKeptOutputBufferHoldsTheNewestBytes(t *testing.T) {
	s := keptServer(t)
	// 102 is not a multiple of the 4-byte writes, so the oldest kept
	// write is cut.
	s.config.Limits.OutputBytes = 102
	first := open(t, s, caller, `{"script":"echo ready; read line; i=0; while [ $i -lt 50 ]; do printf '%04d' $i; i=$((i+1)); done","keep":"long"}`)
	first.until("ready")
	_ = first.stream.Close()
	_ = first.finish()
	k := s.find("long")
	if err := k.write([]byte("go\n")); err != nil {
		t.Fatal(err)
	}
	<-k.ended

	var all strings.Builder
	all.WriteString("ready\n")
	for i := range 50 {
		fmt.Fprintf(&all, "%04d", i)
	}
	want := all.String()[all.Len()-102:]
	second := open(t, s, caller, `{"attach":"long"}`)
	if err := second.finish(); err != nil {
		t.Fatal(err)
	}
	if second.out.String() != want || second.exit != 0 {
		t.Fatalf("buffer: got %q, want %q", second.out.String(), want)
	}
}

func TestSecondAttachTakesOverAndInputEOFStaysInTheSession(t *testing.T) {
	s := keptServer(t)
	first := open(t, s, caller, `{"script":"echo ready; while read line; do echo \"got $line\"; done; echo eof","keep":"cat"}`)
	first.until("ready")
	if err := first.stream.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	k := s.find("cat")
	if k.done() || strings.Contains(buffered(k), "eof") {
		t.Fatal("the end of session input closed the input of the process")
	}

	second := open(t, s, caller, `{"attach":"cat"}`)
	if err := first.finish(); err == nil || err.Error() != "detached" {
		t.Fatalf("the first session: %v", err)
	}
	second.send("x\n")
	second.until("got x")

	reply, err := ask(t, s, caller, map[string]any{"action": "list"})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := reply["rows"].([]any)
	if len(rows) != 1 {
		t.Fatalf("list: %v", reply)
	}
	row, _ := rows[0].([]any)
	if len(row) != 7 || row[0] != "cat" || row[1] != "running" || row[2] != nil || row[3] != true || row[4] != false || row[6] != caller {
		t.Fatalf("list: %v", reply)
	}

	_ = second.stream.Close()
	_ = second.finish()
	pid := k.process.Process.Pid
	reply, err = ask(t, s, caller, map[string]any{"action": "kill", "name": "cat"})
	if err != nil || reply["state"] != "killed" {
		t.Fatalf("kill: %v %v", reply, err)
	}
	if alive(pid) {
		t.Fatal("the process runs after kill")
	}
	if s.find("cat") != nil {
		t.Fatal("the name stays after kill")
	}
	if _, err := ask(t, s, caller, map[string]any{"action": "kill", "name": "cat"}); err == nil || err.Error() != "not_found" {
		t.Fatalf("kill of a free name: %v", err)
	}
}

func TestKeptTerminalKeepsOutputWhileDetached(t *testing.T) {
	s := keptServer(t)
	first := open(t, s, caller, `{"script":"stty -echo; printf ready; read word; sleep 0.3; printf 'tty-%s' \"$word\"; exit 4","pty":true,"rows":24,"cols":80,"keep":"tty"}`)
	first.until("ready")
	first.send("w\n")
	_ = first.stream.Close()
	_ = first.finish()
	k := s.find("tty")
	if k.done() {
		t.Fatal("the terminal process ended with its session")
	}
	<-k.ended

	plain := open(t, s, caller, `{"attach":"tty"}`)
	if err := plain.finish(); err == nil || err.Error() != "pty_mismatch" {
		t.Fatalf("attach without a terminal: %v", err)
	}
	second := open(t, s, caller, `{"attach":"tty","pty":true,"rows":30,"cols":90}`)
	if err := second.finish(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.out.String(), "readytty-w") || second.exit != 4 {
		t.Fatalf("terminal attach: %q exit %d", second.out.String(), second.exit)
	}
}

func TestKeptProcessRefusals(t *testing.T) {
	s := keptServer(t)
	for _, body := range []string{`{"argv":["sleep","9"],"keep":"x"}`, `{"attach":"x"}`} {
		if err := open(t, s, stranger, body).finish(); err == nil || err.Error() != "access_denied" {
			t.Fatalf("%s without a grant: %v", body, err)
		}
	}
	for _, body := range []map[string]any{{"action": "list"}, {"action": "kill", "name": "x"}} {
		if _, err := ask(t, s, stranger, body); err == nil || err.Error() != "access_denied" {
			t.Fatalf("%v without a grant: %v", body, err)
		}
	}
	if s.find("x") != nil {
		t.Fatal("a caller without a grant kept a process")
	}
	if err := open(t, s, caller, `{"attach":"nobody"}`).finish(); err == nil || err.Error() != "not_found" {
		t.Fatalf("attach to an unknown name: %v", err)
	}
	first := open(t, s, caller, `{"argv":["sleep","9"],"keep":"x"}`)
	eventually(t, "the start of x", func() bool { return s.find("x") != nil })
	if err := open(t, s, caller, `{"argv":["true"],"keep":"x"}`).finish(); err == nil || err.Error() != "name_in_use" {
		t.Fatalf("a name in use: %v", err)
	}
	_ = first.stream.Close()
	for _, body := range []map[string]any{{"argv": []string{"true"}, "keep": "y"}, {"attach": "x"}} {
		if _, err := ask(t, s, caller, body); err == nil || err.Error() != "use_exec_session" {
			t.Fatalf("%v as request/reply: %v", body, err)
		}
	}
	stream := execStream(t, s, caller, `{"attach":"x"}`, provider.ServerStream)
	if _, err := io.ReadAll(stream); err == nil || err.Error() != provider.ErrUnsupported.Error() {
		t.Fatalf("attach without input: %v", err)
	}
	if err := open(t, s, caller, `{"argv":["true"],"keep":"t","timeout_ms":5}`).finish(); err == nil {
		t.Fatal("a kept process took a timeout")
	}
	if err := open(t, s, caller, `{"argv":["true"],"keep":"../x"}`).finish(); err == nil {
		t.Fatal("a name with a slash was kept")
	}
}

func TestProviderStopEndsKeptProcesses(t *testing.T) {
	s := keptServer(t)
	first := open(t, s, caller, `{"script":"echo up; sleep 100","keep":"sleeper"}`)
	first.until("up")
	_ = first.stream.Close()
	pid := s.find("sleeper").process.Process.Pid
	s.stopKept()
	if alive(pid) {
		t.Fatal("a kept process outlived the provider")
	}
	if err := open(t, s, caller, `{"argv":["true"],"keep":"late"}`).finish(); err == nil || err.Error() != "provider_stopping" {
		t.Fatalf("keep after stop: %v", err)
	}
}
