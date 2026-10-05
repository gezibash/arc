package server

import (
	"context"
	"errors"
	"testing"

	"github.com/gezibash/arc/sdk/provider"
)

// An app that serves offers must not take files from each caller. A put
// works only after its owner sets a limit.
func TestAPutIsRefusedWithNoLimit(t *testing.T) {
	s := &server{state: t.TempDir(), slots: make(chan struct{}, MaxTransfers)}
	request := provider.Request{
		From:    "1111111111111111111111111111111111111111111111111111111111111111",
		Message: `{"v":1,"op":"put","sha256":"2222222222222222222222222222222222222222222222222222222222222222","size":10,"sdp":{"type":"offer","sdp":"v=0"}}`,
	}
	if _, err := s.HandleRequest(context.Background(), request); !errors.Is(err, errPutRefused) {
		t.Fatalf("a put with no limit gave %v, want %v", err, errPutRefused)
	}
}
