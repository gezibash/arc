// Package proof holds the end-to-end proofs of ARC. Each proof builds the
// real programs, and runs them as a user does: separate processes, separate
// homes, a local relay, and directories that stand in for USB sticks.
package proof

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	// repo is the root of the checkout.
	repo string
	// programs holds the built programs: arc, arc-exec, arc-sqlite and
	// arc-releases.
	programs string
)

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

func run(m *testing.M) int {
	var err error
	if repo, err = filepath.Abs("../.."); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if programs, err = os.MkdirTemp("", "arc-proof-"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(programs)
	for _, name := range []string{"arc", "arc-exec", "arc-sqlite", "arc-releases"} {
		if err := build(filepath.Join(programs, name), "./cmd/"+name, ""); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	return m.Run()
}

// build compiles one package of the checkout to out.
func build(out, pkg, ldflags string) error {
	args := []string{"build"}
	if ldflags != "" {
		args = append(args, "-ldflags", ldflags)
	}
	command := exec.Command("go", append(args, "-o", out, pkg)...)
	command.Dir = repo
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("go build %s: %v\n%s", pkg, err, output)
	}
	return nil
}

// program names one built program.
func program(name string) string { return filepath.Join(programs, name) }

// machine is one home of arc: one machine of a citizen.
type machine struct {
	t       *testing.T
	program string
	home    string
	env     []string
}

// arc gives the machine that keeps its home in dir.
func arc(t *testing.T, dir string) machine {
	return machine{t: t, program: program("arc"), home: dir}
}

// with adds environment variables, each as NAME=value.
func (m machine) with(env ...string) machine {
	m.env = append(append([]string(nil), m.env...), env...)
	return m
}

// using runs another arc program with the same home.
func (m machine) using(program string) machine {
	m.program = program
	return m
}

type result struct {
	Stdout, Stderr string
	Err            error
}

func (m machine) command(args ...string) *exec.Cmd {
	command := exec.Command(m.program, append([]string{"--home", m.home}, args...)...)
	command.Dir = repo
	command.Env = append(os.Environ(), m.env...)
	return command
}

// try runs one command with stdin, and reports what it wrote.
func (m machine) try(stdin string, args ...string) result {
	command := m.command(args...)
	var stdout, stderr bytes.Buffer
	command.Stdin = strings.NewReader(stdin)
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	return result{stdout.String(), stderr.String(), err}
}

// run runs one command that must succeed, and returns its standard output.
func (m machine) run(args ...string) string {
	m.t.Helper()
	return m.input("", args...)
}

// input is run with text on standard input.
func (m machine) input(stdin string, args ...string) string {
	m.t.Helper()
	got := m.try(stdin, args...)
	if got.Err != nil {
		m.t.Fatalf("arc %s: %v\nstdout: %s\nstderr: %s", strings.Join(args, " "), got.Err, got.Stdout, got.Stderr)
	}
	return got.Stdout
}

// refuse runs one command that must fail, and returns its standard error.
func (m machine) refuse(args ...string) string {
	m.t.Helper()
	got := m.try("", args...)
	if got.Err == nil {
		m.t.Fatalf("arc %s succeeded; want a failure\nstdout: %s", strings.Join(args, " "), got.Stdout)
	}
	return got.Stderr
}

// key returns the public key of the identity, in hex.
func (m machine) key() string {
	m.t.Helper()
	return line(m.run("whoami"), 2)
}

// name returns the name of the identity.
func (m machine) name() string {
	m.t.Helper()
	return line(m.run("whoami"), 1)
}

// process is a command that runs in the background.
type process struct {
	command *exec.Cmd
	mu      sync.Mutex
	log     bytes.Buffer
	done    chan struct{}
}

func (p *process) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.Write(data)
}

// output returns what the process wrote to standard output and error.
func (p *process) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.log.String()
}

// waitFor waits until the output holds text.
func (p *process) waitFor(text string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if strings.Contains(p.output(), text) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return strings.Contains(p.output(), text)
}

// stop ends the process and each process that it started.
func (p *process) stop() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGTERM)
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-p.command.Process.Pid, syscall.SIGKILL)
		<-p.done
	}
}

// start runs one command in the background. The test stops it at its end.
func (m machine) start(args ...string) *process {
	m.t.Helper()
	p := &process{command: m.command(args...), done: make(chan struct{})}
	p.command.Stdout, p.command.Stderr = p, p
	// A process group lets stop end the provider program too.
	p.command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.command.Start(); err != nil {
		m.t.Fatalf("arc %s: %v", strings.Join(args, " "), err)
	}
	go func() { _ = p.command.Wait(); close(p.done) }()
	m.t.Cleanup(p.stop)
	return p
}

// serve runs `arc serve` in the background, and waits until it serves.
func (m machine) serve(args ...string) *process {
	m.t.Helper()
	p := m.start(append([]string{"serve"}, args...)...)
	if !p.waitFor("serves", 10*time.Second) {
		m.t.Fatalf("arc serve %s did not serve:\n%s", strings.Join(args, " "), p.output())
	}
	return p
}

// startRelay runs a relay that keeps its events in dir, and returns its URL.
func startRelay(t *testing.T, dir string, flags ...string) string {
	t.Helper()
	p := arc(t, dir).start(append([]string{"relay", "serve", "--listen", "127.0.0.1:0"}, flags...)...)
	if !p.waitFor("relay listens on ", 10*time.Second) {
		t.Fatalf("the relay did not start:\n%s", p.output())
	}
	for _, text := range strings.Split(p.output(), "\n") {
		if url, ok := strings.CutPrefix(text, "relay listens on "); ok {
			return strings.TrimSpace(url)
		}
	}
	t.Fatalf("the relay printed no URL:\n%s", p.output())
	return ""
}

// line returns line n of text, counted from 1, or "" if text is shorter.
func line(text string, n int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if n < 1 || n > len(lines) {
		return ""
	}
	return lines[n-1]
}

// keyfile names the key file of the one identity of a home.
func keyfile(t *testing.T, home string) string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(home, "citizens", "*", "key"))
	if err != nil || len(found) != 1 {
		t.Fatalf("%s holds %d key files, want 1 (%v)", home, len(found), err)
	}
	return found[0]
}

var managedTime = regexp.MustCompile(`(?m)^(created_at|updated_at): (.+)\n`)

// journalDoc checks the two timestamps that ARC adds to a journal page
// against the clock of the test, and returns the page without them. begin is
// the time before the first write of the test.
func journalDoc(t *testing.T, begin time.Time, text string) string {
	t.Helper()
	stamps := map[string]time.Time{}
	for _, match := range managedTime.FindAllStringSubmatch(text, -1) {
		at, err := time.Parse(time.RFC3339, match[2])
		if err != nil {
			t.Fatalf("%s is %q: %v", match[1], match[2], err)
		}
		stamps[match[1]] = at
	}
	created, updated := stamps["created_at"], stamps["updated_at"]
	if created.IsZero() || updated.IsZero() {
		t.Fatalf("the page holds no created_at and updated_at:\n%s", text)
	}
	if created.Before(begin.Add(-time.Second)) || updated.Before(created) || updated.After(time.Now().Add(time.Second)) {
		t.Fatalf("created_at %s and updated_at %s are outside %s .. now", created, updated, begin)
	}
	return managedTime.ReplaceAllString(text, "")
}

// write writes one file, and makes its directory.
func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// read reads one file.
func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
