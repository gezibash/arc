package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// storageCitizen is a storage citizen that a test starts with its own
// rule of admission, on the relay of the tests.
type storageCitizen struct {
	key, dir string
	// owners is STORAGE_OWNERS.
	owners string
}

// startStorage serves the storage app with arc serve, with no
// STORAGE_OWNERS and with the settings of extra, such as STORAGE_ADMIT=ask.
func startStorage(t *testing.T, extra ...string) storageCitizen {
	t.Helper()
	return startStorageOf(t, nil, extra...)
}

// startStorageOf serves the storage app with the account key of each home
// of owners in STORAGE_OWNERS.
func startStorageOf(t *testing.T, owners []string, extra ...string) storageCitizen {
	t.Helper()
	home := newCitizen(t)
	sc := storageCitizen{key: whoami(t, home), dir: filepath.Join(t.TempDir(), "blobs")}
	var accounts []string
	for _, owner := range owners {
		accounts = append(accounts, strings.TrimSpace(arc(t, owner, "keys", "account", sc.key)))
	}
	serve := exec.Command(env.arc, "--home", home, "serve", env.app)
	sc.owners = strings.Join(accounts, ",")
	serve.Env = append(append(os.Environ(), "STORAGE_DIR="+sc.dir, "STORAGE_OWNERS="+sc.owners, "STORAGE_STUN=none", "STORAGE_LOOPBACK=1"), extra...)
	served := &output{}
	serve.Stdout, serve.Stderr = served, served
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = serve.Process.Kill()
		_ = serve.Wait()
		if t.Failed() {
			t.Logf("the log of the storage citizen:\n%s", served.String())
		}
	})
	if !waitUntil(15*time.Second, func() bool { return strings.Contains(served.String(), "serves storage") }) {
		t.Fatalf("the app did not start:\n%s", served.String())
	}
	return sc
}

// newCitizen makes an arc home with a new key and the relay of the tests.
func newCitizen(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "home")
	arc(t, home, "keys", "gen")
	arc(t, home, "relay", "add", env.relay)
	return home
}

func whoami(t *testing.T, home string) string {
	t.Helper()
	return strings.Split(arc(t, home, "whoami"), "\n")[1]
}

// arc runs the arc program in a home, and fails the test if it fails.
func arc(t *testing.T, home string, args ...string) string {
	t.Helper()
	out, err := arcRun(home, args...)
	if err != nil {
		t.Fatalf("arc %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func arcRun(home string, args ...string) (string, error) {
	out, err := exec.Command(env.arc, append([]string{"--home", home}, args...)...).CombinedOutput()
	return string(out), err
}

// operate runs a command of the operator on the disk of a storage citizen.
func operate(t *testing.T, sc storageCitizen, args ...string) string {
	t.Helper()
	command := exec.Command(env.program, args...)
	command.Env = append(os.Environ(), "STORAGE_DIR="+sc.dir, "STORAGE_OWNERS="+sc.owners)
	var out, errs bytes.Buffer
	command.Stdout, command.Stderr = &out, &errs
	if err := command.Run(); err != nil {
		t.Fatalf("arc-storage %s: %v\n%s", strings.Join(args, " "), err, errs.String())
	}
	return out.String()
}

// join runs arc keys storage add, and returns its output and error.
func join(home string, args ...string) (string, error) {
	return arcRun(home, append([]string{"keys", "storage", "add"}, args...)...)
}

func mustJoin(t *testing.T, home string, want string, args ...string) {
	t.Helper()
	out, err := join(home, args...)
	if err != nil || !strings.Contains(out, want) {
		t.Fatalf("arc keys storage add %s: %v\n%s\nwant %q", strings.Join(args, " "), err, out, want)
	}
}

func mustRefuse(t *testing.T, home string, want string, args ...string) {
	t.Helper()
	out, err := join(home, args...)
	if err == nil || !strings.Contains(out, want) {
		t.Fatalf("arc keys storage add %s: %v\n%s\nwant a refusal with %q", strings.Join(args, " "), err, out, want)
	}
}

// TestInviteAdmits checks the default rule: a join with no token, with a
// wrong token and with a spent token is refused. A join with a good token
// is admitted at the quota and the plan of the token.
func TestInviteAdmits(t *testing.T) {
	sc := startStorage(t)
	alice, bob := newCitizen(t), newCitizen(t)

	mustRefuse(t, alice, "refused the account: this storage citizen admits only with an invite", sc.key)
	wrong := make([]byte, 16)
	if _, err := rand.Read(wrong); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, alice, "the invite token is unknown, spent, or older than 7 days",
		fmt.Sprintf("storage+arc://%s?invite=%s", sc.key, hex.EncodeToString(wrong)))

	link := strings.TrimSpace(operate(t, sc, "invite", "-key", sc.key, "-quota", "64KiB", "-plan", "friend"))
	if !strings.HasPrefix(link, "storage+arc://"+sc.key+"?invite=") {
		t.Fatalf("invite printed %q", link)
	}
	mustJoin(t, alice, "storage: admitted: 0 B of 64 KiB in use, plan friend, open", link)
	if out := mustRun(t, alice, "usage"); !strings.HasPrefix(out, "0 bytes of 65536 in use, 0 blobs, plan friend, open") {
		t.Fatalf("usage printed %q", out)
	}
	// A second join of the account gets its state, and spends nothing.
	mustJoin(t, alice, "storage: admitted: 0 B of 64 KiB in use", sc.key)
	// The token is spent.
	mustRefuse(t, bob, "the invite token is unknown, spent, or older than 7 days", link)

	// The account takes puts, and is named by the account key.
	path, _ := file(t, 100)
	putIDs(t, alice, path)
	account := strings.TrimSpace(arc(t, alice, "keys", "account", sc.key))
	if _, err := os.Stat(filepath.Join(sc.dir, "accounts", account, "state.json")); err != nil {
		t.Fatalf("the account of the account key: %v", err)
	}
}

// TestAskRule checks the rule ask: the join is pending, the operator sees
// it, and the next join after accept is admitted. A refused request stays
// refused.
func TestAskRule(t *testing.T) {
	sc := startStorage(t, "STORAGE_ADMIT=ask")
	carol, dave := newCitizen(t), newCitizen(t)

	mustJoin(t, carol, "storage: pending: the operator decides by hand", sc.key, "--note", "carol from the choir", "--show-key", "--want", "128KiB")
	mustJoin(t, carol, "storage: pending", sc.key)
	account := strings.TrimSpace(arc(t, carol, "keys", "account", sc.key))
	listed := operate(t, sc, "requests")
	want := fmt.Sprintf("%s\t131072\t", account)
	if !strings.Contains(listed, want) || !strings.Contains(listed, whoami(t, carol)) || !strings.Contains(listed, "carol from the choir") {
		t.Fatalf("requests printed %q, want the account %s, 131072 bytes, the citizen key and the note", listed, account)
	}
	arc(t, carol, "install", sc.key, "storage", "--yes")
	if _, errs, err := run(carol, "usage"); err == nil || !strings.Contains(errs, "not an owner") {
		t.Fatalf("a pending account opened a session: %v\n%s", err, errs)
	}

	operate(t, sc, "accept", account, "-quota", "128KiB")
	mustJoin(t, carol, "storage: admitted: 0 B of 128 KiB in use, plan free, open", sc.key)
	if out := operate(t, sc, "requests"); out != "" {
		t.Fatalf("requests after accept printed %q", out)
	}

	mustJoin(t, dave, "storage: pending", sc.key)
	operate(t, sc, "refuse", strings.TrimSpace(arc(t, dave, "keys", "account", sc.key)))
	mustRefuse(t, dave, "refused the account: the operator refused this account", sc.key)
}

// TestOpenRuleQuotaEachAccount checks the rule open, and the quota of each
// account: a put over the quota of one account fails while another
// account has room. The operator then raises the quota, and closes an
// account.
func TestOpenRuleQuotaEachAccount(t *testing.T) {
	sc := startStorage(t, "STORAGE_ADMIT=open", "STORAGE_OPEN_QUOTA=8KiB", "STORAGE_CAPACITY=16KiB")
	erin, frank, gina := newCitizen(t), newCitizen(t), newCitizen(t)

	mustJoin(t, erin, "storage: admitted: 0 B of 8 KiB in use, plan free, open", sc.key)
	mustJoin(t, frank, "storage: admitted: 0 B of 8 KiB in use", sc.key)
	mustRefuse(t, gina, "the storage citizen is full: its accounts hold quotas of 16384 of 16384 bytes", sc.key)

	big, _ := file(t, 5000) // a blob of 8 KiB
	putIDs(t, erin, big)
	small, _ := file(t, 100) // a blob of 4 KiB
	_, errs, err := run(erin, "put", small)
	if want := "over quota: the account uses 8192 of 8192 bytes, and this blob needs 4096 more"; err == nil || !strings.Contains(errs, want) {
		t.Fatalf("a put over the quota of erin: %v\n%s\nwant %q", err, errs, want)
	}
	putIDs(t, frank, small)
	if out := mustRun(t, erin, "usage"); !strings.HasPrefix(out, "8192 bytes of 8192 in use, 1 blobs, plan free, open") {
		t.Fatalf("usage of erin printed %q", out)
	}
	if out := mustRun(t, frank, "usage"); !strings.HasPrefix(out, "4096 bytes of 8192 in use, 1 blobs, plan free, open") {
		t.Fatalf("usage of frank printed %q", out)
	}

	erinAccount := strings.TrimSpace(arc(t, erin, "keys", "account", sc.key))
	operate(t, sc, "account", "set", erinAccount, "-quota", "16KiB", "-plan", "friend")
	putIDs(t, erin, small)
	if out := mustRun(t, erin, "usage"); !strings.HasPrefix(out, "12288 bytes of 16384 in use, 2 blobs, plan friend, open") {
		t.Fatalf("usage of erin after account set printed %q", out)
	}

	frankAccount := strings.TrimSpace(arc(t, frank, "keys", "account", sc.key))
	operate(t, sc, "account", "close", frankAccount)
	other, _ := file(t, 200)
	if _, errs, err := run(frank, "put", other); err == nil || !strings.Contains(errs, "the account is closing: it takes no put") {
		t.Fatalf("a put on a closing account: %v\n%s", err, errs)
	}
	if out := mustRun(t, frank, "usage"); !strings.Contains(out, "plan free, closing") {
		t.Fatalf("usage of a closing account printed %q", out)
	}
	if entries := listAfter(t, frank, "0"); len(entries) != 1 {
		t.Fatalf("a closing account lists %v, want its one blob", entries)
	}
}

// backdateClose moves the close of an account back by the 30 days of its
// reads, as the app would see it 30 days after the close.
func backdateClose(t *testing.T, sc storageCitizen, account string) {
	t.Helper()
	path := filepath.Join(sc.dir, "accounts", account, "state.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(raw, &state); err != nil {
		t.Fatal(err)
	}
	record := state["record"].(map[string]any)
	record["closing"] = record["closing"].(float64) - 30*24*60*60
	if raw, err = json.Marshal(state); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClosedAccountRemoved checks the end of a close by the operator. When
// the reads of the closed account end, the next call removes its
// directory, and its quota is free. A join then admits the same account
// key again, as a new account. An account of STORAGE_OWNERS keeps its
// state.json, so it does not come back by itself; accept opens it again.
func TestClosedAccountRemoved(t *testing.T) {
	owner, frank, gina := newCitizen(t), newCitizen(t), newCitizen(t)
	sc := startStorageOf(t, []string{owner}, "STORAGE_ADMIT=open", "STORAGE_QUOTA=8KiB", "STORAGE_CAPACITY=16KiB")
	ownerAccount := strings.TrimSpace(arc(t, owner, "keys", "account", sc.key))
	frankAccount := strings.TrimSpace(arc(t, frank, "keys", "account", sc.key))

	mustJoin(t, frank, "storage: admitted: 0 B of 8 KiB in use, plan free, open", sc.key)
	mustRefuse(t, gina, "the storage citizen is full", sc.key)
	small, _ := file(t, 100)
	putIDs(t, frank, small)
	arc(t, owner, "install", sc.key, "storage", "--yes")
	putIDs(t, owner, small)

	operate(t, sc, "account", "close", frankAccount)
	operate(t, sc, "account", "close", ownerAccount)
	if _, err := os.Stat(filepath.Join(sc.dir, "accounts", frankAccount, "index.db")); err != nil {
		t.Fatalf("a closing account lost its index before its reads end: %v", err)
	}
	backdateClose(t, sc, frankAccount)
	backdateClose(t, sc, ownerAccount)

	// The join of gina is the next call. The quota of frank is free, and
	// its directory goes.
	mustJoin(t, gina, "storage: admitted: 0 B of 8 KiB in use, plan free, open", sc.key)
	if _, err := os.Stat(filepath.Join(sc.dir, "accounts", frankAccount)); !os.IsNotExist(err) {
		t.Fatalf("the directory of the closed account of frank: %v; want it removed", err)
	}
	ownerDir := filepath.Join(sc.dir, "accounts", ownerAccount)
	for _, gone := range []string{"index.db", "blobs"} {
		if _, err := os.Stat(filepath.Join(ownerDir, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s of the closed owner account: %v; want it removed", gone, err)
		}
	}
	if _, err := os.Stat(filepath.Join(ownerDir, "state.json")); err != nil {
		t.Fatalf("the closed owner account lost its record: %v", err)
	}
	if _, errs, err := run(owner, "usage"); err == nil || !strings.Contains(errs, "not an owner") {
		t.Fatalf("the closed owner account opened a session: %v\n%s", err, errs)
	}

	// Frank is a stranger again: a join admits a new, empty account.
	mustJoin(t, frank, "storage: admitted: 0 B of 8 KiB in use, plan free, open", sc.key)
	if out := mustRun(t, frank, "usage"); !strings.HasPrefix(out, "0 bytes of 8192 in use, 0 blobs, plan free, open") {
		t.Fatalf("usage of the new account of frank printed %q", out)
	}

	operate(t, sc, "accept", ownerAccount, "-quota", "64KiB")
	if out := mustRun(t, owner, "usage"); !strings.HasPrefix(out, "0 bytes of 65536 in use, 0 blobs, plan free, open") {
		t.Fatalf("usage of the owner account after accept printed %q", out)
	}
	putIDs(t, owner, small)
}
