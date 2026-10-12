package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// auditLines runs an audit as the owner, and returns the result of each
// file: pass, fail or lost.
func auditLines(t *testing.T, files ...string) ([]string, string, error) {
	t.Helper()
	out, errs, err := run(env.owner, append([]string{"audit"}, files...)...)
	var results []string
	for line := range strings.Lines(out) {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			t.Fatalf("audit printed %q, want <id> <result> <file>", line)
		}
		results = append(results, fields[1])
	}
	return results, errs, err
}

func blobFile(id string) string {
	return filepath.Join(env.dir, "accounts", env.accountKey, "blobs", id)
}

// TestAudit audits blobs through the real app. A held blob passes. A blob
// with one changed byte fails, and a blob whose file is gone is lost. The
// owner gets no blob for the audit.
func TestAudit(t *testing.T) {
	held, _ := file(t, 300)
	changed, _ := file(t, 400)
	lost, _ := file(t, 500)
	ids := putIDs(t, env.owner, held, changed, lost)
	defer cleanup(t, env.owner, ids...)

	results, errs, err := auditLines(t, held, changed, lost)
	if err != nil || strings.Join(results, " ") != "pass pass pass" {
		t.Fatalf("the audit of three held blobs gave %v: %v\n%s", results, err, errs)
	}

	// The storage citizen changes one byte of a blob, and loses the file
	// of another. Its index still names both.
	path := blobFile(ids[1])
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobFile(ids[2])); err != nil {
		t.Fatal(err)
	}

	results, errs, err = auditLines(t, held, changed, lost)
	if strings.Join(results, " ") != "pass fail lost" {
		t.Fatalf("the audit after the loss gave %v, want pass fail lost\n%s", results, errs)
	}
	if err == nil || !strings.Contains(errs, "2 of 3 blobs failed the audit") {
		t.Fatalf("the audit after the loss did not fail: %v\n%s", err, errs)
	}
}

// TestDrain drains the account of the owner. It refuses a put, and keeps
// each read, list and usage. The end of the drain takes puts again.
func TestDrain(t *testing.T) {
	kept, keptData := file(t, 600)
	ids := putIDs(t, env.owner, kept)
	defer func() { cleanup(t, env.owner, ids...) }()
	defer func() { _, _, _ = run(env.owner, "drain", "-off") }()

	if out := mustRun(t, env.owner, "drain"); out != "draining: 1 blobs of 4096 bytes remain\n" {
		t.Fatalf("drain printed %q", out)
	}
	next, _ := file(t, 700)
	if _, errs, err := run(env.second, "put", next); err == nil || !strings.Contains(errs, "the account is draining") {
		t.Fatalf("a put on a draining account: %v\n%s", err, errs)
	}
	if out := mustRun(t, env.second, "usage"); !strings.Contains(out, "1 blobs, plan owner, draining") {
		t.Fatalf("usage of a draining account printed %q", out)
	}
	if entries := listAfter(t, env.second, "0"); len(entries) == 0 || entries[len(entries)-1].id != ids[0] {
		t.Fatalf("the list of a draining account is %v, want %s last", entries, ids[0])
	}
	got := filepath.Join(t.TempDir(), "kept")
	mustRun(t, env.second, "get", "-o", got, ids[0])
	if data, err := os.ReadFile(got); err != nil || string(data) != string(keptData) {
		t.Fatalf("the get from a draining account gave other bytes: %v", err)
	}
	if results, errs, err := auditLines(t, kept); err != nil || len(results) != 1 || results[0] != "pass" {
		t.Fatalf("the audit of a draining account gave %v: %v\n%s", results, err, errs)
	}

	if out := mustRun(t, env.owner, "drain", "-off"); out != "open: 1 blobs of 4096 bytes remain\n" {
		t.Fatalf("drain -off printed %q", out)
	}
	ids = append(ids, putIDs(t, env.owner, next)...)
}
