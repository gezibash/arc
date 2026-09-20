package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/go/provider"
)

const caller = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const stranger = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func testServer(t *testing.T) *server {
	t.Helper()

	dir := t.TempDir()
	cfg := &config{
		Grants:  map[string]bool{caller: true},
		CWD:     dir,
		Limits:  defaultLimits,
		JobsDir: filepath.Join(dir, "jobs"),
	}
	return &server{config: cfg, lease: newLease(nil, io.Discard), log: io.Discard}
}

func ask(t *testing.T, s *server, from string, body map[string]any) (map[string]any, error) {
	t.Helper()

	message, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	reply, err := s.HandleRequest(context.Background(), provider.Request{
		From:    from,
		Message: string(message),
		Meta:    map[string]any{"method": "EXEC"},
	})
	if err != nil {
		return nil, err
	}

	var result map[string]any
	if err := json.Unmarshal([]byte(reply), &result); err != nil {
		t.Fatalf("the reply is not JSON: %q", reply)
	}
	return result, nil
}

func TestRunsACommand(t *testing.T) {
	s := testServer(t)

	result, err := ask(t, s, caller, map[string]any{"argv": []string{"echo", "hello"}})
	if err != nil {
		t.Fatal(err)
	}
	if result["stdout"] != "hello\n" || result["exit"] != float64(0) {
		t.Errorf("result = %v", result)
	}
	if result["timed_out"] != false || result["truncated"] != false {
		t.Errorf("result = %v", result)
	}
}

func TestRunsAScriptAndReadsStandardInput(t *testing.T) {
	s := testServer(t)

	result, err := ask(t, s, caller, map[string]any{"script": "cat; echo done", "stdin": "input"})
	if err != nil {
		t.Fatal(err)
	}
	if result["stdout"] != "inputdone\n" {
		t.Errorf("stdout = %q", result["stdout"])
	}
}

func TestReportsAnExitCodeAndStandardError(t *testing.T) {
	s := testServer(t)

	result, err := ask(t, s, caller, map[string]any{"script": "echo bad >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if result["exit"] != float64(3) || result["stderr"] != "bad\n" {
		t.Errorf("result = %v", result)
	}
}

func TestStopsACommandAtTheTimeout(t *testing.T) {
	s := testServer(t)

	started := time.Now()
	result, err := ask(t, s, caller, map[string]any{"script": "sleep 30", "timeout_ms": 300})
	if err != nil {
		t.Fatal(err)
	}
	if result["timed_out"] != true {
		t.Errorf("result = %v", result)
	}
	if time.Since(started) > 10*time.Second {
		t.Error("the command ran past its timeout")
	}
}

// A timeout stops the children of the command as well.
func TestStopsTheWholeProcessGroup(t *testing.T) {
	s := testServer(t)
	marker := filepath.Join(t.TempDir(), "alive")

	script := "(sleep 5; touch " + marker + ") & sleep 30"
	if _, err := ask(t, s, caller, map[string]any{"script": script, "timeout_ms": 300}); err != nil {
		t.Fatal(err)
	}

	time.Sleep(6 * time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a child of the command outlived the timeout")
	}
}

func TestCutsOutputToTheBudget(t *testing.T) {
	s := testServer(t)
	s.config.Limits.OutputBytes = 100

	result, err := ask(t, s, caller, map[string]any{"script": "head -c 5000 /dev/zero | tr '\\0' 'x'"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result["stdout"].(string)) != 100 || result["truncated"] != true {
		t.Errorf("stdout is %d characters, truncated %v", len(result["stdout"].(string)), result["truncated"])
	}
}

func TestRunsInTheDirectoryOfTheRequest(t *testing.T) {
	s := testServer(t)
	inside := filepath.Join(s.config.CWD, "inside")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := ask(t, s, caller, map[string]any{"script": "pwd", "cwd": "inside"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result["stdout"].(string), "inside") {
		t.Errorf("pwd = %q", result["stdout"])
	}

	if _, err := ask(t, s, caller, map[string]any{"script": "pwd", "cwd": "missing"}); err == nil {
		t.Error("a directory that is not there passed")
	}
}

func TestRefusesACallerWithoutAGrant(t *testing.T) {
	s := testServer(t)

	_, err := ask(t, s, stranger, map[string]any{"argv": []string{"echo", "hello"}})
	if err == nil || err.Error() != "access_denied" {
		t.Errorf("error = %v, want access_denied", err)
	}
}

func TestRefusesAnotherMethod(t *testing.T) {
	s := testServer(t)

	_, err := s.HandleRequest(context.Background(), provider.Request{
		From:    caller,
		Message: `{"argv":["echo"]}`,
		Meta:    map[string]any{"method": "GET"},
	})
	if err == nil || err.Error() != "invalid_request" {
		t.Errorf("error = %v, want invalid_request", err)
	}
}

func TestRefusesABadBody(t *testing.T) {
	s := testServer(t)

	cases := map[string]string{
		"not JSON":              `not json`,
		"a list":                `["echo"]`,
		"no command":            `{}`,
		"argv and script":       `{"argv":["echo"],"script":"echo"}`,
		"an empty argv":         `{"argv":[]}`,
		"an unknown field":      `{"argv":["echo"],"colour":"red"}`,
		"an unknown action":     `{"action":"delete","argv":["echo"]}`,
		"a timeout of zero":     `{"argv":["echo"],"timeout_ms":0}`,
		"a job that is not one": `{"action":"status","job":"nope"}`,
	}

	for name, message := range cases {
		_, err := s.HandleRequest(context.Background(), provider.Request{
			From:    caller,
			Message: message,
			Meta:    map[string]any{"method": "EXEC"},
		})
		if err == nil {
			t.Errorf("%s: the request passed", name)
		}
	}
}

func TestRefusesABodyOverTheLimit(t *testing.T) {
	s := testServer(t)
	s.config.Limits.BodyBytes = 50

	_, err := ask(t, s, caller, map[string]any{"script": strings.Repeat("x", 200)})
	if err == nil || err.Error() != "request_too_large" {
		t.Errorf("error = %v, want request_too_large", err)
	}
}

func TestAJobRunsAndReportsItsResult(t *testing.T) {
	s := testServer(t)

	started, err := ask(t, s, caller, map[string]any{"action": "start", "script": "echo working; sleep 1; echo done"})
	if err != nil {
		t.Fatal(err)
	}

	id, _ := started["job"].(string)
	if !jobIDPattern.MatchString(id) || started["state"] != "running" {
		t.Fatalf("start = %v", started)
	}

	running, err := ask(t, s, caller, map[string]any{"action": "status", "job": id})
	if err != nil {
		t.Fatal(err)
	}
	if running["state"] != "running" {
		t.Errorf("state = %v, want running", running["state"])
	}

	deadline := time.Now().Add(20 * time.Second)
	var final map[string]any
	for time.Now().Before(deadline) {
		final, err = ask(t, s, caller, map[string]any{"action": "status", "job": id})
		if err != nil {
			t.Fatal(err)
		}
		if final["state"] == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	if final["state"] != "done" {
		t.Fatalf("the job did not end: %v", final)
	}
	if final["exit"] != float64(0) || !strings.Contains(final["stdout"].(string), "done") {
		t.Errorf("result = %v", final)
	}
	if final["ended_at"] == nil || final["started_at"] == nil {
		t.Errorf("the times are missing: %v", final)
	}
}

// A job of another caller looks the same as a job that is not there.
func TestAJobIsPrivateToItsCaller(t *testing.T) {
	s := testServer(t)
	s.config.Grants[stranger] = true

	started, err := ask(t, s, caller, map[string]any{"action": "start", "script": "sleep 1"})
	if err != nil {
		t.Fatal(err)
	}

	_, err = ask(t, s, stranger, map[string]any{"action": "status", "job": started["job"]})
	if err == nil || err.Error() != "not_found" {
		t.Errorf("error = %v, want not_found", err)
	}

	_, err = ask(t, s, caller, map[string]any{"action": "status", "job": "000000000000-00000000"})
	if err == nil || err.Error() != "not_found" {
		t.Errorf("error = %v, want not_found", err)
	}
}

func TestTheLeaseHoldsWhileWorkRuns(t *testing.T) {
	s := testServer(t)

	var calls []string
	held := newLease(&leaseConfig{Hold: []string{"hold"}, Release: []string{"release"}, IntervalMS: 1000}, io.Discard)
	held.run = func(argv []string) { calls = append(calls, argv[0]) }
	s.lease = held

	if _, err := ask(t, s, caller, map[string]any{"argv": []string{"true"}}); err != nil {
		t.Fatal(err)
	}

	if len(calls) != 2 || calls[0] != "hold" || calls[1] != "release" {
		t.Errorf("the lease ran %v", calls)
	}
}

func TestTheNotifyCommandCarriesTheResult(t *testing.T) {
	s := testServer(t)
	written := filepath.Join(t.TempDir(), "notified")
	s.config.Notify = &notifyConfig{
		Argv:      []string{"sh", "-c", "cat > " + written + ".{owner}"},
		TimeoutMS: 30_000,
	}

	started, err := ask(t, s, caller, map[string]any{"action": "start", "script": "echo finished"})
	if err != nil {
		t.Fatal(err)
	}

	path := written + "." + caller
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the notify command did not run: %v", err)
	}

	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("the notify body is not JSON: %s", data)
	}
	if result["job"] != started["job"] || result["state"] != "done" {
		t.Errorf("the notify body is %v", result)
	}
}

func TestLoadConfigChecksTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	good := `{"grants":["` + caller + `"],"cwd":"` + dir + `"}`
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EXEC_CONFIG", path)

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Grants[caller] || cfg.CWD != dir || cfg.Limits != defaultLimits {
		t.Errorf("config = %+v", cfg)
	}

	bad := map[string]string{
		"no grants":                `{"grants":[]}`,
		"a key that is short":      `{"grants":["abc"]}`,
		"an unknown field":         `{"grants":["` + caller + `"],"colour":"red"}`,
		"a relative cwd":           `{"grants":["` + caller + `"],"cwd":"relative"}`,
		"a limit over the ceiling": `{"grants":["` + caller + `"],"limits":{"timeout_ms":999999999}}`,
		"an unknown limit":         `{"grants":["` + caller + `"],"limits":{"forever":1}}`,
		"half a lease":             `{"grants":["` + caller + `"],"lease":{"hold":["x"],"interval_ms":1000}}`,
		"a short interval":         `{"grants":["` + caller + `"],"lease":{"hold":["x"],"release":["y"],"interval_ms":10}}`,
		"notify without argv":      `{"grants":["` + caller + `"],"notify":{"timeout_ms":1000}}`,
	}

	for name, body := range bad {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(); err == nil {
			t.Errorf("%s: the configuration passed", name)
		}
	}
}
