package program

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
)

// signer builds arc-signer once for the tests of this package.
func signer(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "arc-signer")
	if out, err := exec.Command("go", "build", "-o", path, "github.com/gezibash/arc/cmd/arc-signer").CombinedOutput(); err != nil {
		t.Fatalf("go build arc-signer: %v\n%s", err, out)
	}
	return path
}

// run runs arc-signer with stdin, and returns its standard output.
func run(t *testing.T, program, stdin string, env []string, args ...string) (string, error) {
	t.Helper()
	command := exec.Command(program, args...)
	command.Stdin = strings.NewReader(stdin)
	command.Env = append(os.Environ(), env...)
	var out, errs bytes.Buffer
	command.Stdout, command.Stderr = &out, &errs
	err := command.Run()
	if err != nil {
		return errs.String(), err
	}
	return strings.TrimSpace(out.String()), nil
}

// TestProgramSigner makes a key with arc-signer, and asks it over its
// lines through the Client, as arc does.
func TestProgramSigner(t *testing.T) {
	program := signer(t)
	dir := t.TempDir()
	public, err := run(t, program, "", nil, "gen", "--dir", dir)
	if err != nil {
		t.Fatalf("gen: %v: %s", err, public)
	}
	pk, err := nostr.PubKeyFromHex(public)
	if err != nil {
		t.Fatalf("gen printed %q, not a public key", public)
	}
	client := &Client{Program: program, Dir: dir}
	t.Cleanup(func() { _ = client.Close() })
	r := keys.Remote{Public: pk, Call: client.Call}
	ctx := context.Background()

	got, err := client.Call(ctx, "get_public_key")
	if err != nil || got != public {
		t.Fatalf("get_public_key: %q, %v", got, err)
	}
	event := nostr.Event{Kind: 1, Content: "through the lines", CreatedAt: nostr.Now()}
	if err := r.SignEvent(ctx, &event); err != nil {
		t.Fatal(err)
	}
	if root, err := r.KeyedRoot(ctx); err != nil || len(root) != 32 {
		t.Fatalf("keyed root: %x, %v", root, err)
	}
	if _, err := client.Call(ctx, "no_such_method"); err == nil {
		t.Fatal("an unknown method got an answer")
	}
	// The program answers again after a refusal.
	if got, err := client.Call(ctx, "ping"); err != nil || got != "pong" {
		t.Fatalf("ping after a refusal: %q, %v", got, err)
	}
	// The key file of the program holds the secret; nothing else in the
	// folder does.
	if _, err := os.Stat(filepath.Join(dir, "signer", "key")); err != nil {
		t.Fatal(err)
	}
}

// TestSealedKey seals the key, and checks that the program opens it only
// with the passphrase.
func TestSealedKey(t *testing.T) {
	program := signer(t)
	dir := t.TempDir()
	k := keys.Generate()
	public, err := run(t, program, k.Secret.Hex(), nil, "import", "--dir", dir)
	if err != nil || public != k.Public.Hex() {
		t.Fatalf("import printed %q, %v; want %s", public, err, k.Public.Hex())
	}
	if out, err := run(t, program, "", []string{"ARC_PASSPHRASE=open sesame"}, "encrypt", "--dir", dir); err != nil {
		t.Fatalf("encrypt: %v: %s", err, out)
	}
	text, err := os.ReadFile(filepath.Join(dir, "signer", "key"))
	if err != nil || !strings.HasPrefix(string(text), "ncryptsec1") {
		t.Fatalf("the key is not sealed: %q, %v", text, err)
	}
	request := `{"id":"1","method":"get_public_key","params":[]}` + "\n"
	if out, err := run(t, program, request, []string{"ARC_PASSPHRASE=open sesame"}, "stdio", "--dir", dir); err != nil || !strings.Contains(out, k.Public.Hex()) {
		t.Fatalf("with the passphrase: %q, %v", out, err)
	}
	if out, err := run(t, program, request, []string{"ARC_PASSPHRASE=wrong"}, "stdio", "--dir", dir); err == nil {
		t.Fatalf("a wrong passphrase opened the key: %q", out)
	}
}

// TestFind refuses a name that is a path, so a key file cannot name any
// program on the disk.
func TestFind(t *testing.T) {
	for _, name := range []string{"", "../arc-signer", "/bin/sh", "a/b"} {
		if _, err := Find(name); err == nil {
			t.Errorf("Find(%q) took a path", name)
		}
	}
}

// TestALeftRequestKeepsTheSigner leaves requests whose context ends, as a
// live caller of arc serve does when it goes away. The signer keeps
// answering the next callers, in order.
func TestALeftRequestKeepsTheSigner(t *testing.T) {
	program := signer(t)
	dir := t.TempDir()
	if out, err := run(t, program, "", nil, "gen", "--dir", dir); err != nil {
		t.Fatalf("gen: %v: %s", err, out)
	}
	client := &Client{Program: program, Dir: dir}
	t.Cleanup(func() { _ = client.Close() })
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for range 20 {
		_, _ = client.Call(gone, "get_public_key")
		if got, err := client.Call(context.Background(), "ping"); err != nil || got != "pong" {
			t.Fatalf("after a left request, ping gave %q, %v", got, err)
		}
	}
}

// TestAKeyOfAnotherIdentity refuses to answer for an identity whose public
// key is not the key in signer/key, as after a migration that took a stale
// public file.
func TestAKeyOfAnotherIdentity(t *testing.T) {
	program := signer(t)
	dir := t.TempDir()
	if out, err := run(t, program, "", nil, "gen", "--dir", dir); err != nil {
		t.Fatalf("gen: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "public"), []byte(keys.Generate().Public.Hex()+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	request := `{"id":"1","method":"get_public_key","params":[]}` + "\n"
	if out, err := run(t, program, request, nil, "stdio", "--dir", dir); err == nil || !strings.Contains(out, "and the identity is") {
		t.Fatalf("a key of another identity answered: %q, %v", out, err)
	}
}
