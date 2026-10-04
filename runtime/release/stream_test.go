package release_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/release"
	"github.com/gezibash/arc/sdk/provider"
	"github.com/gezibash/arc/sdk/providertest"
)

type streamedArchive struct {
	t         *testing.T
	data      string
	final     error
	requested string
}

func (s *streamedArchive) Request(context.Context, []byte) ([]byte, error) {
	s.t.Error("stream fell back to a chunk request")
	return nil, errors.New("unexpected chunk")
}
func (s *streamedArchive) OpenArchive(_ context.Context, digest string) (release.ArchiveReader, error) {
	s.requested = digest
	return providertest.Start(s.t, session.ServerStream, func(_ context.Context, stream *session.Stream) error {
		if _, err := io.WriteString(stream, s.data); err != nil {
			return err
		}
		if err := stream.CloseWrite(); err != nil {
			return err
		}
		return s.final
	}), nil
}
func TestStreamedArchiveRequiresSizeHashAndFinalSuccess(t *testing.T) {
	hash := sha256.Sum256([]byte("trusted archive"))
	digest := hex.EncodeToString(hash[:])
	for _, tc := range []struct {
		name, data string
		final      error
		valid      bool
	}{
		{"valid", "trusted archive", nil, true},
		{"short", "trusted", nil, false},
		{"long", "trusted archive!", nil, false},
		{"corrupt", "altered archive", nil, false},
		{"final failure", "trusted archive", provider.Error("storage_failure"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := &streamedArchive{t: t, data: tc.data, final: tc.final}
			last := int64(0)
			result, err := release.Download(context.Background(), source, &release.Artifact{Size: 15, SHA256: digest}, func(n, total int64) {
				if n < last || n > total {
					t.Errorf("invalid progress %d/%d", n, total)
				}
				last = n
			})
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v got=%q err=%v", tc.valid, result, err)
			}
			if source.requested != "sha256:"+digest {
				t.Fatal(source.requested)
			}
			if tc.valid && (string(result) != "trusted archive" || last != 15) {
				t.Fatalf("result=%q progress=%d", result, last)
			}
		})
	}
}
