package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/provider"
	"github.com/gezibash/arc/go/sealedbox"
)

func testFiles(t *testing.T) (*server, *identity.Identity) {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "objects"), 0o700); err != nil {
		t.Fatal(err)
	}

	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return &server{store: &store{root: root, quota: DefaultQuota}}, me
}

// seal makes a token that only the owner opens.
func seal(t *testing.T, me *identity.Identity, text string) string {
	t.Helper()

	sealed, err := sealedbox.SealTo(me.PublicKey, []byte(text))
	if err != nil {
		t.Fatal(err)
	}
	return "sealed-v1:" + base64Std(sealed)
}

// makeEnvelope builds one signed envelope, as the CLI of a citizen does.
func makeEnvelope(t *testing.T, me *identity.Identity, name, body string) *envelope {
	t.Helper()

	owner := me.EncodePublicKey()
	sealedName := seal(t, me, name)
	sealedBody := seal(t, me, body)
	bodyHash := sum(sealedBody)

	message := signatureMessage(owner, sealedName, bodyHash)
	signature := hex.EncodeToString(me.Sign([]byte(message)))

	return &envelope{
		Version:   1,
		Owner:     owner,
		Name:      sealedName,
		BodyHash:  bodyHash,
		Signature: signature,
		ID:        sum(message + "\n" + signature),
		Body:      sealedBody,
	}
}

func call(t *testing.T, s *server, from string, body map[string]any) (map[string]any, error) {
	t.Helper()

	message, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	reply, err := s.HandleRequest(context.Background(), provider.Request{
		From: from, Message: string(message),
		Meta: map[string]any{"method": "RAW", "path": "/files"},
	})
	if err != nil {
		return nil, err
	}

	var answer map[string]any
	if err := json.Unmarshal([]byte(reply), &answer); err != nil {
		t.Fatalf("the reply is not JSON: %q", reply)
	}
	return answer, nil
}

func TestPutGetAndList(t *testing.T) {
	s, me := testFiles(t)
	owner := me.EncodePublicKey()
	held := makeEnvelope(t, me, "plan.md", "the body")

	answer, err := call(t, s, owner, map[string]any{"op": "put", "file": held})
	if err != nil {
		t.Fatal(err)
	}
	if answer["id"] != held.ID {
		t.Errorf("put = %v", answer)
	}

	// The same envelope again is not an error.
	if _, err := call(t, s, owner, map[string]any{"op": "put", "file": held}); err != nil {
		t.Errorf("the second put failed: %v", err)
	}

	got, err := call(t, s, owner, map[string]any{"op": "get", "id": held.ID})
	if err != nil {
		t.Fatal(err)
	}

	file, _ := got["file"].(map[string]any)
	if file["body"] != held.Body || file["owner"] != owner {
		t.Errorf("get = %v", file)
	}

	listed, err := call(t, s, owner, map[string]any{"op": "list"})
	if err != nil {
		t.Fatal(err)
	}

	files, _ := listed["files"].([]any)
	if len(files) != 1 {
		t.Fatalf("the list holds %v", files)
	}

	// A listing never carries a body.
	header, _ := files[0].(map[string]any)
	if _, held := header["body"]; held {
		t.Error("the listing carries a body")
	}
	if header["id"] != held.ID {
		t.Errorf("the header is %v", header)
	}
}

func TestAnotherCitizenSeesNothing(t *testing.T) {
	s, me := testFiles(t)
	other, _ := identity.Generate()
	held := makeEnvelope(t, me, "plan.md", "the body")

	if _, err := call(t, s, me.EncodePublicKey(), map[string]any{"op": "put", "file": held}); err != nil {
		t.Fatal(err)
	}

	if _, err := call(t, s, other.EncodePublicKey(), map[string]any{"op": "get", "id": held.ID}); err == nil {
		t.Error("another citizen read the file")
	}

	listed, err := call(t, s, other.EncodePublicKey(), map[string]any{"op": "list"})
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := listed["files"].([]any); len(files) != 0 {
		t.Errorf("another citizen saw %v", files)
	}
}

func TestAnEnvelopeThatDoesNotHold(t *testing.T) {
	s, me := testFiles(t)
	owner := me.EncodePublicKey()

	changes := map[string]func(*envelope){
		"another version":   func(e *envelope) { e.Version = 2 },
		"another owner":     func(e *envelope) { e.Owner = strings.Repeat("ab", 32) },
		"a changed body":    func(e *envelope) { e.Body = seal(t, me, "another body") },
		"a changed hash":    func(e *envelope) { e.BodyHash = strings.Repeat("cd", 32) },
		"a changed name":    func(e *envelope) { e.Name = seal(t, me, "another name") },
		"a name not sealed": func(e *envelope) { e.Name = "plan.md" },
		"a body not sealed": func(e *envelope) { e.Body = "the body" },
		"a changed id":      func(e *envelope) { e.ID = strings.Repeat("ef", 32) },
		"no signature":      func(e *envelope) { e.Signature = "" },
	}

	for name, change := range changes {
		held := makeEnvelope(t, me, "plan.md", "the body")
		change(held)

		if _, err := call(t, s, owner, map[string]any{"op": "put", "file": held}); err == nil {
			t.Errorf("%s: the envelope passed", name)
		}
	}
}

// An envelope signed by one citizen does not land in the store of another.
func TestAnEnvelopeOfAnotherOwner(t *testing.T) {
	s, me := testFiles(t)
	other, _ := identity.Generate()
	held := makeEnvelope(t, other, "plan.md", "the body")

	if _, err := call(t, s, me.EncodePublicKey(), map[string]any{"op": "put", "file": held}); err == nil {
		t.Error("the envelope of another citizen landed")
	}
}

func TestTheQuotaHoldsAWrite(t *testing.T) {
	s, me := testFiles(t)
	s.store.quota = 100
	owner := me.EncodePublicKey()

	held := makeEnvelope(t, me, "plan.md", "the body")
	if _, err := call(t, s, owner, map[string]any{"op": "put", "file": held}); err == nil {
		t.Error("a write over the quota passed")
	}

	// A retry of an envelope that landed already passes, even when the owner
	// is full.
	s.store.quota = DefaultQuota
	if _, err := call(t, s, owner, map[string]any{"op": "put", "file": held}); err != nil {
		t.Fatal(err)
	}

	s.store.quota = 10
	if _, err := call(t, s, owner, map[string]any{"op": "put", "file": held}); err != nil {
		t.Errorf("the retry of a stored envelope failed: %v", err)
	}
}

func TestAnEnvelopeNeverChanges(t *testing.T) {
	s, me := testFiles(t)
	owner := me.EncodePublicKey()
	held := makeEnvelope(t, me, "plan.md", "the body")

	if _, err := call(t, s, owner, map[string]any{"op": "put", "file": held}); err != nil {
		t.Fatal(err)
	}

	// Another envelope under the same id conflicts. Only a write straight to
	// the disk can make one.
	other := makeEnvelope(t, me, "other.md", "another body")
	other.ID = held.ID

	data, _ := json.Marshal(other)
	if _, err := same(mustRead(t, s.store.path(owner, held.ID)), data, other); err == nil {
		t.Error("another envelope passed as the same")
	}
}

func TestARequestThatIsNotOne(t *testing.T) {
	s, me := testFiles(t)
	owner := me.EncodePublicKey()

	for _, body := range []map[string]any{
		{"op": "delete"},
		{"op": "put"},
		{"op": "get", "id": "short"},
		{"op": "list", "after": "short"},
		{},
	} {
		if _, err := call(t, s, owner, body); err == nil {
			t.Errorf("%v passed", body)
		}
	}

	if _, err := s.HandleRequest(context.Background(), provider.Request{
		From: "not-a-key", Message: `{"op":"list"}`, Meta: map[string]any{},
	}); err == nil {
		t.Error("a caller that is not a key passed")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
