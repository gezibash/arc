package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/providertest"
)

func archiveStream(t *testing.T, s *server, digest string) *provider.Stream {
	body, _ := json.Marshal(map[string]string{"op": "archive", "digest": digest})
	return providertest.Start(t, provider.ServerStream, func(ctx context.Context, stream *provider.Stream) error {
		return s.HandleSession(ctx, provider.Request{Message: string(body), Meta: map[string]any{"method": "RAW", "path": "/releases"}}, stream)
	})
}
func TestReleaseSessionArchiveAndPathBoundary(t *testing.T) {
	s, root := testReleases(t)
	data := []byte(strings.Repeat("archive", 10000))
	hash := sha256.Sum256(data)
	digest := hex.EncodeToString(hash[:])
	path := filepath.Join(root, "blobs", digest+".tar.gz")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	stream := archiveStream(t, s, "sha256:"+digest)
	got, err := io.ReadAll(stream)
	if err != nil || string(got) != string(data) || stream.Wait() != nil {
		t.Fatalf("stream bytes=%d err=%v", len(got), err)
	}
	for _, bad := range []string{"sha256:../../secret", "sha256:" + strings.Repeat("f", 64)} {
		if _, err := io.ReadAll(archiveStream(t, s, bad)); err == nil {
			t.Fatalf("accepted %s", bad)
		}
	}
	linked := strings.Repeat("a", 64)
	if err := os.Symlink(path, filepath.Join(root, "blobs", linked+".tar.gz")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(archiveStream(t, s, "sha256:"+linked)); err == nil {
		t.Fatal("followed symlink")
	}
}
