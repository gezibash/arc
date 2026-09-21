package relay

import (
	"crypto/sha256"
	"net"
	"sync"
	"testing"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/internal/wire"
	"github.com/gezibash/arc/packet"
	"github.com/gezibash/arc/session"
)

// Many senders on one link put their messages on the wire in the order of
// their sequence numbers. The partner refuses a message out of order.
func TestALinkSendsInOrder(t *testing.T) {
	me, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	partner, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}

	talk, err := session.Establish(me, partner.PublicKey)
	if err != nil {
		t.Fatal(err)
	}

	near, far := net.Pipe()
	t.Cleanup(func() { near.Close(); far.Close() })

	held := &link{
		peer: partner.PublicKey, conn: near, outbound: true,
		ready: true, channel: make([]byte, sha256.Size), talk: talk,
	}
	f := &federation{me: me}

	const count = 200

	var group sync.WaitGroup
	for range count {
		group.Add(1)
		go func() {
			defer group.Done()
			f.write(held, kindForward, "", []byte("in order"))
		}()
	}

	for want := range uint64(count) {
		payload, err := wire.ReadFrame(far, 1<<20)
		if err != nil {
			t.Fatalf("message %d: %v", want, err)
		}
		body, ok := isFederationFrame(payload)
		if !ok {
			t.Fatalf("message %d is not a federation frame", want)
		}
		decoded, err := packet.Decode(body)
		if err != nil {
			t.Fatalf("message %d: %v", want, err)
		}
		if decoded.Seq != want {
			t.Fatalf("message %d carries sequence %d", want, decoded.Seq)
		}
	}
	group.Wait()
}
