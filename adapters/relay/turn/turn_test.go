package turn

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
	"github.com/pion/stun/v4"
	pturn "github.com/pion/turn/v5"
)

// start runs a TURN server on a loopback socket, and its HTTP route.
func start(t *testing.T, quota int64) (*Server, *httptest.Server) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Serve(conn, Config{Secret: "secret", PublicIP: net.IPv4(127, 0, 0, 1), RelayHost: "127.0.0.1", MinPort: 50000, MaxPort: 50999, Quota: quota})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	web := httptest.NewServer(s)
	t.Cleanup(web.Close)
	return s, web
}

// get asks the HTTP route for a credential with the given authorization.
func get(t *testing.T, web *httptest.Server, authorization string) (int, Credential) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, web.URL+Path, nil)
	if authorization != "" {
		request.Header.Set("Authorization", authorization)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var credential Credential
	if response.StatusCode == http.StatusOK {
		if err := json.NewDecoder(response.Body).Decode(&credential); err != nil {
			t.Fatal(err)
		}
	}
	return response.StatusCode, credential
}

// signed makes a NIP-98 header from an event that the test changes before
// it signs.
func signed(t *testing.T, k keys.Key, change func(*nostr.Event)) string {
	t.Helper()
	event := nostr.Event{Kind: KindHTTPAuth, CreatedAt: nostr.Now()}
	change(&event)
	if err := k.SignEvent(context.Background(), &event); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(event)
	return "Nostr " + base64.StdEncoding.EncodeToString(data)
}

// TestCredential checks that only a valid NIP-98 request gets a credential,
// and that the credential names the key that signed the request.
func TestCredential(t *testing.T) {
	_, web := start(t, 1000)
	k := keys.Generate()
	url := web.URL + Path

	header, err := AuthHeader(context.Background(), k, http.MethodGet, url)
	if err != nil {
		t.Fatal(err)
	}
	status, credential := get(t, web, header)
	if status != http.StatusOK {
		t.Fatalf("a valid request got %d", status)
	}
	if !strings.HasSuffix(credential.Username, ":"+k.Public.Hex()) {
		t.Errorf("username %q does not name the key %s", credential.Username, k.Public.Hex())
	}
	if credential.QuotaLeft != 1000 || credential.TTL != 3600 || len(credential.URLs) != 1 || !strings.HasPrefix(credential.URLs[0], "turn:127.0.0.1:") {
		t.Errorf("credential %+v", credential)
	}

	tampered := signed(t, k, func(e *nostr.Event) { e.Tags = nostr.Tags{{"u", url}, {"method", "GET"}} })
	tampered = strings.Replace(tampered, "Nostr ", "", 1)
	data, _ := base64.StdEncoding.DecodeString(tampered)
	var event nostr.Event
	_ = json.Unmarshal(data, &event)
	event.Tags = nostr.Tags{{"u", url}, {"method", "GET"}, {"x", "changed after the signature"}}
	data, _ = json.Marshal(event)

	for name, authorization := range map[string]string{
		"no header":     "",
		"bad signature": "Nostr " + base64.StdEncoding.EncodeToString(data),
		"expired": signed(t, k, func(e *nostr.Event) {
			e.CreatedAt = nostr.Timestamp(time.Now().Add(-2 * time.Minute).Unix())
			e.Tags = nostr.Tags{{"u", url}, {"method", "GET"}}
		}),
		"other URL":    signed(t, k, func(e *nostr.Event) { e.Tags = nostr.Tags{{"u", "https://other.example/turn"}, {"method", "GET"}} }),
		"other method": signed(t, k, func(e *nostr.Event) { e.Tags = nostr.Tags{{"u", url}, {"method", "POST"}} }),
		"other kind": signed(t, k, func(e *nostr.Event) {
			e.Kind = 22242
			e.Tags = nostr.Tags{{"u", url}, {"method", "GET"}}
		}),
	} {
		if status, _ := get(t, web, authorization); status != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, status)
		}
	}
}

// TestQuota relays bytes from a peer to a client until the quota of the
// client is used. Then the relay drops the rest, and refuses a new
// allocation.
func TestQuota(t *testing.T) {
	const quota = 10_000
	s, web := start(t, quota)
	k := keys.Generate()
	header, err := AuthHeader(context.Background(), k, http.MethodGet, web.URL+Path)
	if err != nil {
		t.Fatal(err)
	}
	_, credential := get(t, web, header)
	server := strings.TrimSuffix(strings.TrimPrefix(credential.URLs[0], "turn:"), "?transport=udp")

	allocate := func() (*pturn.Client, net.PacketConn, error) {
		conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client, err := pturn.NewClient(&pturn.ClientConfig{TURNServerAddr: server, Username: credential.Username, Password: credential.Password, Realm: Realm, Conn: conn})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Listen(); err != nil {
			t.Fatal(err)
		}
		relayed, err := client.Allocate()
		if err != nil {
			client.Close()
			_ = conn.Close()
			return nil, nil, err
		}
		t.Cleanup(func() { _ = relayed.Close(); client.Close(); _ = conn.Close() })
		return client, relayed, nil
	}
	_, relayed, err := allocate()
	if err != nil {
		t.Fatal(err)
	}

	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	// The client writes first, so the relay lets the peer in.
	if _, err := relayed.WriteTo([]byte("hello"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := peer.ReadFrom(buf); err != nil {
		t.Fatalf("the peer got nothing through the relay: %v", err)
	}

	// The peer sends 30 packets of 1,000 bytes: three times the quota.
	go func() {
		packet := make([]byte, 1000)
		for range 30 {
			_, _ = peer.WriteTo(packet, relayed.LocalAddr())
			time.Sleep(5 * time.Millisecond)
		}
	}()
	received := 0
	for {
		_ = relayed.SetReadDeadline(time.Now().Add(time.Second))
		n, _, err := relayed.ReadFrom(buf)
		if err != nil {
			break
		}
		received += n
	}
	// "hello" spent 5 bytes, so 9 packets fit; the tenth goes over.
	if received != 9000 {
		t.Errorf("the client got %d bytes through the relay, want 9000", received)
	}
	if left := s.Left(k.Public.Hex()); left != 0 {
		t.Errorf("left %d, want 0", left)
	}
	if _, credential := get(t, web, header); credential.QuotaLeft != 0 {
		t.Errorf("quota_left %d, want 0", credential.QuotaLeft)
	}
	if _, _, err := allocate(); err == nil {
		t.Error("the relay gave a second allocation after the quota was used")
	}
}

// TestBinding checks that the TURN server answers a STUN binding request
// with no credential, as the STUN server of a relay does. The expected
// address is the address of the client socket, from the kernel.
func TestBinding(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, err := Serve(conn, Config{Secret: "secret", PublicIP: net.IPv4(127, 0, 0, 1), RelayHost: "127.0.0.1", MinPort: 50000, MaxPort: 50999})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	client, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	request := stun.MustBuild(stun.TransactionID, stun.BindingRequest, stun.Fingerprint)
	if _, err := client.Write(request.Raw); err != nil {
		t.Fatal(err)
	}
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("no reply: %v", err)
	}
	reply := &stun.Message{Raw: buf[:n]}
	if err := reply.Decode(); err != nil {
		t.Fatal(err)
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(reply); err != nil {
		t.Fatal(err)
	}
	want := client.LocalAddr().(*net.UDPAddr)
	if !mapped.IP.Equal(want.IP) || mapped.Port != want.Port {
		t.Errorf("mapped address %s, want %s", mapped, want)
	}
}

// TestRelayToRelay sends packets between two allocations on one relay. The
// relay sockets listen on 127.0.0.1 behind the public address 192.0.2.1,
// where nothing listens, as on Fly.io, where the network drops a packet
// from the relay to its own public address (#264).
func TestRelayToRelay(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	public := net.IPv4(192, 0, 2, 1)
	s, err := Serve(conn, Config{Secret: "secret", PublicIP: public, RelayHost: "127.0.0.1", MinPort: 51000, MaxPort: 51999, Quota: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	allocate := func() (string, net.PacketConn) {
		k := keys.Generate()
		credential, err := s.Credential(k.Public.Hex(), "127.0.0.1")
		if err != nil {
			t.Fatal(err)
		}
		c, err := net.ListenPacket("udp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		client, err := pturn.NewClient(&pturn.ClientConfig{TURNServerAddr: conn.LocalAddr().String(), Username: credential.Username, Password: credential.Password, Realm: Realm, Conn: c})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.Listen(); err != nil {
			t.Fatal(err)
		}
		relayed, err := client.Allocate()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = relayed.Close(); client.Close(); _ = c.Close() })
		return k.Public.Hex(), relayed
	}
	keyA, a := allocate()
	keyB, b := allocate()
	for _, r := range []net.PacketConn{a, b} {
		if ip := r.LocalAddr().(*net.UDPAddr).IP; !ip.Equal(public) {
			t.Fatalf("relayed address %s, want the public address %s", r.LocalAddr(), public)
		}
	}

	// B writes first, so the relay lets A in. A has no permission for B
	// yet, so the relay drops this packet after it counts it.
	if _, err := b.WriteTo([]byte("B->A"), a.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := a.WriteTo([]byte("A->B"), b.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	_ = b.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err := b.ReadFrom(buf)
	if err != nil {
		t.Fatalf("B got nothing from A: %v", err)
	}
	if string(buf[:n]) != "A->B" || from.String() != a.LocalAddr().String() {
		t.Errorf("B got %q from %s, want %q from %s", buf[:n], from, "A->B", a.LocalAddr())
	}

	// A has a permission for B now, so B reaches A too.
	if _, err := b.WriteTo([]byte("B->A"), a.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	_ = a.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, from, err = a.ReadFrom(buf)
	if err != nil {
		t.Fatalf("A got nothing from B: %v", err)
	}
	if string(buf[:n]) != "B->A" || from.String() != b.LocalAddr().String() {
		t.Errorf("A got %q from %s, want %q from %s", buf[:n], from, "B->A", b.LocalAddr())
	}

	// A relay socket counts each packet that it sends or gets against its
	// own citizen, as for a peer on the network. Each of A and B sent or
	// got three packets of 4 bytes.
	for _, key := range []string{keyA, keyB} {
		if used := 1<<20 - s.Left(key); used != 12 {
			t.Errorf("citizen %s used %d bytes, want 12", key[:8], used)
		}
	}
}

// A TURN server with a TCP listener, as behind a TLS proxy on port 443,
// gives allocations over TCP too. The relay sockets stay UDP, the quota
// counts their bytes, and the credential names the extra URL.
func TestAnAllocationOverTCP(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	const extra = "turns:relay.example:443?transport=tcp"
	s, err := Serve(conn, Config{Secret: "secret", PublicIP: net.IPv4(127, 0, 0, 1), RelayHost: "127.0.0.1", MinPort: 51000, MaxPort: 51999, Quota: 1 << 20, TCP: tcp, URLs: []string{extra}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	k := keys.Generate()
	credential, err := s.Credential(k.Public.Hex(), "relay.example")
	if err != nil {
		t.Fatal(err)
	}
	if len(credential.URLs) != 2 || credential.URLs[1] != extra || !strings.HasPrefix(credential.URLs[0], "turn:relay.example:") {
		t.Fatalf("the credential names %q, want the UDP URL and %q", credential.URLs, extra)
	}

	stream, err := net.Dial("tcp4", tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client, err := pturn.NewClient(&pturn.ClientConfig{TURNServerAddr: tcp.Addr().String(), Username: credential.Username, Password: credential.Password, Realm: Realm, Conn: pturn.NewSTUNConn(stream)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	if err := client.Listen(); err != nil {
		t.Fatal(err)
	}
	relayed, err := client.Allocate()
	if err != nil {
		t.Fatalf("no allocation over TCP: %v", err)
	}
	t.Cleanup(func() { _ = relayed.Close() })
	if _, ok := relayed.LocalAddr().(*net.UDPAddr); !ok {
		t.Fatalf("the relayed address is %v, want UDP", relayed.LocalAddr())
	}

	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if _, err := relayed.WriteTo([]byte("hello"), peer.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, _, err := peer.ReadFrom(buf); err != nil || string(buf[:n]) != "hello" {
		t.Fatalf("the peer got %q through the relay: %v", buf[:n], err)
	}
	if left := s.Left(k.Public.Hex()); left != 1<<20-5 {
		t.Errorf("left %d after 5 relayed bytes, want %d", left, 1<<20-5)
	}
}
