package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/provider"
	wire "github.com/gezibash/arc/sdk/storage"
	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
)

// TestEmulatedLinkPuts puts blobs through the storage app on an emulated
// link, and prints the time. It is a measurement, not a check: it fails
// only if a put fails. It runs only if STORAGE_LINK_PUTS is set.
//
// The link is the link of TestEmulatedLinkSpeed in sdk/direct:
// DIRECT_LINK_MBIT (default 92.5), DIRECT_LINK_RTT_MS (default 44) and
// DIRECT_LINK_QUEUE_KIB (default 128). STORAGE_LINK_PUTS is the count of
// puts, and STORAGE_LINK_KIB is the size of each blob (default 4).
//
//	STORAGE_LINK_PUTS=1000 go test ./apps/storage/server -run TestEmulatedLinkPuts -count=1 -v
func TestEmulatedLinkPuts(t *testing.T) {
	puts := int(envFloat(t, "STORAGE_LINK_PUTS", 0))
	if puts == 0 {
		t.Skip("set STORAGE_LINK_PUTS to measure puts on an emulated link")
	}
	size := int(envFloat(t, "STORAGE_LINK_KIB", 4) * 1024)
	mbit := envFloat(t, "DIRECT_LINK_MBIT", 92.5)
	rtt := time.Duration(envFloat(t, "DIRECT_LINK_RTT_MS", 44) * float64(time.Millisecond))
	queue := int(envFloat(t, "DIRECT_LINK_QUEUE_KIB", 128) * 1024)

	router, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: "10.0.0.0/24", MinDelay: rtt / 2, LoggerFactory: logging.NewDefaultLoggerFactory()})
	if err != nil {
		t.Fatal(err)
	}
	clientNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.0.0.1"}})
	if err != nil {
		t.Fatal(err)
	}
	appNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"10.0.0.2"}})
	if err != nil {
		t.Fatal(err)
	}
	// The bytes of a put go to the app, so the rate limit is on its side.
	limited, err := vnet.NewQueue(appNet, vnet.NewTBFQueue(int(mbit*1e6), 16<<10, int64(queue)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = limited.Close() })
	if err := router.AddNet(clientNet); err != nil {
		t.Fatal(err)
	}
	if err := router.AddNet(limited); err != nil {
		t.Fatal(err)
	}
	if err := router.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Stop() })

	account := strings.Repeat("a", 64)
	s := &server{
		store: &store{dir: t.TempDir(), owners: []string{account}, quota: DefaultQuota, now: time.Now},
		rtc:   direct.Options{Net: appNet},
		slots: make(chan struct{}, MaxSessions),
		log:   io.Discard,
	}
	client := wire.Client{
		Call: func(ctx context.Context, session string) ([]byte, error) {
			open := wire.Open{Version: 1}
			if err := json.Unmarshal([]byte(session), &open.Session); err != nil {
				return nil, err
			}
			body, err := json.Marshal(open)
			if err != nil {
				return nil, err
			}
			reply, err := s.HandleRequest(ctx, provider.Request{From: account, Message: string(body)})
			return []byte(reply), err
		},
		Options: direct.Options{Net: clientNet},
		NoRelay: true,
	}
	ctx := context.Background()
	conn, err := client.Open(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	blobs := make([]wire.Blob, puts)
	for i := range blobs {
		data := make([]byte, size)
		_, _ = rand.Read(data)
		sum := sha256.Sum256(data)
		blobs[i] = wire.Blob{ID: hex.EncodeToString(sum[:]), Data: data}
	}
	start := time.Now()
	stored := putAll(ctx, t, conn, blobs)
	seconds := time.Since(start).Seconds()
	if stored != puts {
		t.Fatalf("%d of %d puts stored", stored, puts)
	}
	fmt.Printf("link %.1f Mbit/s, RTT %v, queue %d KiB: %d puts of %d bytes in %.2f s, %.1f puts/s\n",
		mbit, rtt, queue>>10, puts, size, seconds, float64(puts)/seconds)
}

func envFloat(t *testing.T, name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return f
}

func putAll(ctx context.Context, t *testing.T, conn *wire.Conn, blobs []wire.Blob) int {
	results, err := conn.PutMany(ctx, blobs)
	if err != nil {
		t.Fatal(err)
	}
	stored := 0
	for _, r := range results {
		if r.Err != nil {
			t.Fatal(r.Err)
		}
		if r.Status == wire.StatusStored {
			stored++
		}
	}
	return stored
}
