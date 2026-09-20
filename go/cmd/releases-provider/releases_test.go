package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/provider"
)

func testReleases(t *testing.T) (*server, string) {
	t.Helper()

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "channels"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "blobs"), 0o700); err != nil {
		t.Fatal(err)
	}
	return &server{root: root}, root
}

func ask(t *testing.T, s *server, body map[string]any) (string, error) {
	t.Helper()

	message, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return s.HandleRequest(context.Background(), provider.Request{
		Message: string(message),
		Meta:    map[string]any{"method": "RAW", "path": "/releases"},
	})
}

func TestReadsAChannelDocument(t *testing.T) {
	s, root := testReleases(t)
	document := `{"version":"0.6.0","artifact":"sha256:abc"}`

	if err := os.WriteFile(filepath.Join(root, "channels", "stable.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}

	reply, err := ask(t, s, map[string]any{"op": "channel", "channel": "stable"})
	if err != nil {
		t.Fatal(err)
	}
	if reply != document {
		t.Errorf("reply = %q", reply)
	}

	if _, err := ask(t, s, map[string]any{"op": "channel", "channel": "beta"}); err == nil {
		t.Error("a channel that is not there passed")
	}
	if _, err := ask(t, s, map[string]any{"op": "channel", "channel": "nightly"}); err == nil {
		t.Error("a channel that does not exist passed")
	}
}

func TestReadsAnArchiveInChunks(t *testing.T) {
	s, root := testReleases(t)

	body := []byte(strings.Repeat("arc", 1000))
	digest := sha256.Sum256(body)
	name := hex.EncodeToString(digest[:])

	if err := os.WriteFile(filepath.Join(root, "blobs", name+".tar.gz"), body, 0o600); err != nil {
		t.Fatal(err)
	}

	reply, err := ask(t, s, map[string]any{
		"op": "chunk", "digest": "sha256:" + name, "offset": 0, "length": 10,
	})
	if err != nil {
		t.Fatal(err)
	}

	var answer map[string]any
	if err := json.Unmarshal([]byte(reply), &answer); err != nil {
		t.Fatal(err)
	}

	data, err := base64.StdEncoding.DecodeString(answer["data"].(string))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "arcarcarca" {
		t.Errorf("the chunk is %q", data)
	}
	if answer["digest"] != "sha256:"+name || answer["offset"] != float64(0) {
		t.Errorf("the answer is %v", answer)
	}

	// A chunk at the end returns what is left, and no more.
	reply, err = ask(t, s, map[string]any{
		"op": "chunk", "digest": "sha256:" + name, "offset": len(body) - 2, "length": 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal([]byte(reply), &answer)
	data, _ = base64.StdEncoding.DecodeString(answer["data"].(string))
	if string(data) != "rc" {
		t.Errorf("the last chunk is %q", data)
	}
}

func TestRefusesARequestThatIsNotOne(t *testing.T) {
	s, root := testReleases(t)

	body := []byte("x")
	digest := sha256.Sum256(body)
	name := hex.EncodeToString(digest[:])
	os.WriteFile(filepath.Join(root, "blobs", name+".tar.gz"), body, 0o600)

	cases := []map[string]any{
		{"op": "chunk", "digest": "sha256:short", "offset": 0, "length": 10},
		{"op": "chunk", "digest": name, "offset": 0, "length": 10},
		{"op": "chunk", "digest": "sha256:" + name, "offset": -1, "length": 10},
		{"op": "chunk", "digest": "sha256:" + name, "offset": 0, "length": 0},
		{"op": "chunk", "digest": "sha256:" + name, "offset": 0, "length": MaxChunkBytes + 1},
		{"op": "chunk", "digest": "sha256:" + name, "offset": 1000, "length": 10},
		{"op": "chunk", "digest": "sha256:" + strings.Repeat("ab", 32), "offset": 0, "length": 10},
		{"op": "write", "channel": "stable"},
		{"op": "channel", "channel": "../../etc/passwd"},
	}

	for _, body := range cases {
		if _, err := ask(t, s, body); err == nil {
			t.Errorf("%v passed", body)
		}
	}
}
