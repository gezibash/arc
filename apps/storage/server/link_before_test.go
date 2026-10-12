package server

import (
	"context"
	"testing"

	wire "github.com/gezibash/arc/sdk/storage"
)

func putAll(ctx context.Context, t *testing.T, conn *wire.Conn, blobs []wire.Blob) int {
	stored := 0
	for _, b := range blobs {
		status, err := conn.Put(ctx, b.ID, b.Data)
		if err != nil {
			t.Fatal(err)
		}
		if status == wire.StatusStored {
			stored++
		}
	}
	return stored
}
