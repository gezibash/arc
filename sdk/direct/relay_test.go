package direct

import (
	"bytes"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/pion/logging"
	"github.com/pion/transport/v5/vnet"
	"github.com/pion/webrtc/v4"
)

// symmetric is a NAT that maps each destination to a new port, and lets in
// only packets from the destination of a mapping. Two ends behind such
// NATs find no direct path.
var symmetric = &vnet.NATType{
	MappingBehavior:   vnet.EndpointAddrPortDependent,
	FilteringBehavior: vnet.EndpointAddrPortDependent,
}

// natPair makes a WAN with a relay at 1.2.3.4, and two ends, each behind
// its own symmetric NAT. The relay answers STUN and TURN on port 3478.
func natPair(t *testing.T) (a, b *vnet.Net, relay *turn.Server) {
	t.Helper()
	wan, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: "1.2.3.0/24", LoggerFactory: logging.NewDefaultLoggerFactory()})
	if err != nil {
		t.Fatal(err)
	}
	relayNet, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{"1.2.3.4"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := wan.AddNet(relayNet); err != nil {
		t.Fatal(err)
	}
	end := func(public, cidr, private string) *vnet.Net {
		lan, err := vnet.NewRouter(&vnet.RouterConfig{CIDR: cidr, StaticIPs: []string{public}, NATType: symmetric, LoggerFactory: logging.NewDefaultLoggerFactory()})
		if err != nil {
			t.Fatal(err)
		}
		if err := wan.AddRouter(lan); err != nil {
			t.Fatal(err)
		}
		n, err := vnet.NewNet(&vnet.NetConfig{StaticIPs: []string{private}})
		if err != nil {
			t.Fatal(err)
		}
		if err := lan.AddNet(n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	a = end("1.2.3.10", "10.1.0.0/24", "10.1.0.2")
	b = end("1.2.3.20", "10.2.0.0/24", "10.2.0.2")
	if err := wan.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wan.Stop() })

	conn, err := relayNet.ListenPacket("udp4", "1.2.3.4:3478")
	if err != nil {
		t.Fatal(err)
	}
	relay, err = turn.Serve(conn, turn.Config{
		Secret: "secret", PublicIP: net.IPv4(1, 2, 3, 4), RelayHost: "1.2.3.4",
		MinPort: 49152, MaxPort: 49251, Quota: 1 << 30, Net: relayNet,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.Close() })
	return a, b, relay
}

// transfer opens a connection from a to b and sends size bytes on it. It
// returns the path, or an error if the data channel did not open.
func transfer(t *testing.T, a, b Options, size int64) (string, bool) {
	t.Helper()
	sender, err := NewConnection(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sender.Close() }()
	receiver, err := NewConnection(b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = receiver.Close() }()

	done := make(chan int64, 1)
	receiver.OnDataChannel(func(dc *webrtc.DataChannel) {
		var got int64
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if m.IsString {
				done <- got
				return
			}
			got += int64(len(m.Data))
		})
	})
	dc, err := sender.CreateDataChannel("file", nil)
	if err != nil {
		t.Fatal(err)
	}
	opened := make(chan struct{})
	dc.OnOpen(func() { close(opened) })

	offer, err := sender.CreateOffer(nil)
	if err == nil {
		offer, err = Describe(sender, offer)
	}
	if err == nil {
		err = receiver.SetRemoteDescription(offer)
	}
	answer, err2 := receiver.CreateAnswer(nil)
	if err == nil && err2 == nil {
		answer, err = Describe(receiver, answer)
	}
	if err == nil {
		err = sender.SetRemoteDescription(answer)
	}
	if err != nil || err2 != nil {
		t.Fatal(err, err2)
	}

	select {
	case <-opened:
	case <-time.After(OpenWait):
		return "", false
	}
	path := SelectedPath(sender)
	if err := Send(dc, bytes.NewReader(make([]byte, size)), size); err != nil {
		t.Fatal(err)
	}
	if err := dc.SendText("end"); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got != size {
			t.Fatalf("received %d bytes, want %d", got, size)
		}
	case <-time.After(time.Minute):
		t.Fatal("the receiver did not get the file")
	}
	return path, true
}

// TestRelayPath puts two ends behind symmetric NATs. The direct attempt
// with the STUN server of the relay finds no path. The second attempt,
// with the TURN server of the relay on one end, sends the file through the
// relay.
func TestRelayPath(t *testing.T) {
	a, b, relay := natPair(t)
	const stun = "stun:1.2.3.4:3478"

	// The STUN server of the relay gives each end an address behind its
	// NAT, so the direct attempt fails because of the NATs.
	for _, n := range []*vnet.Net{a, b} {
		pc, err := NewConnection(Options{STUN: stun, Net: n})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pc.CreateDataChannel("probe", nil); err != nil {
			t.Fatal(err)
		}
		offer, err := pc.CreateOffer(nil)
		if err == nil {
			offer, err = Describe(pc, offer)
		}
		_ = pc.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(offer.SDP, "typ srflx") {
			t.Fatalf("an end got no address from the STUN server of the relay:\n%s", offer.SDP)
		}
	}

	if path, ok := transfer(t, Options{STUN: stun, Net: a}, Options{STUN: stun, Net: b}, 64<<10); ok {
		t.Fatalf("the direct attempt found the path %q through two symmetric NATs", path)
	}

	key := "0000000000000000000000000000000000000000000000000000000000000001"
	credential, err := relay.Credential(key, "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	server := &webrtc.ICEServer{URLs: credential.URLs, Username: credential.Username, Credential: credential.Password}
	const size = 1 << 20
	path, ok := transfer(t, Options{TURN: server, Net: a}, Options{STUN: stun, Net: b}, size)
	if !ok {
		t.Fatal("the attempt with TURN found no path")
	}
	if path != "relay" {
		t.Errorf("path %q, want relay", path)
	}
	// The relay counted the file, and the packets of SCTP and DTLS.
	if used := int64(1<<30) - relay.Left(key); used < size {
		t.Errorf("the relay counted %d bytes, less than the file of %d", used, size)
	}
}

// TestRelayPathWithoutSTUN checks the attempt through the relay when the
// end with no TURN server gets no answer from its own STUN server. The TURN
// server lets in only packets from the addresses in the answer, and the
// answer then holds only an address behind the NAT. The end finds its
// public address with the STUN server of the TURN server, which the other
// end names.
func TestRelayPathWithoutSTUN(t *testing.T) {
	a, b, relay := natPair(t)
	credential, err := relay.Credential("0000000000000000000000000000000000000000000000000000000000000002", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	server := &webrtc.ICEServer{URLs: credential.URLs, Username: credential.Username, Credential: credential.Password}

	if path, ok := transfer(t, Options{TURN: server, Net: a}, Options{Net: b}, 64<<10); ok {
		t.Fatalf("the path %q opened, but the TURN server does not know the public address of the other end", path)
	}

	stun, err := TURNSTUN(t.Context(), server)
	if err != nil {
		t.Fatal(err)
	}
	if stun != "stun:1.2.3.4:3478" {
		t.Fatalf("STUN URL %q of the TURN server, want stun:1.2.3.4:3478", stun)
	}
	path, ok := transfer(t, Options{TURN: server, Net: a}, Options{PeerSTUN: stun, Net: b}, 1<<20)
	if !ok {
		t.Fatal("the attempt with the STUN server of the TURN server found no path")
	}
	if path != "relay" {
		t.Errorf("path %q, want relay", path)
	}
}

// TestADeadFirstSTUNServerDoesNotStopAnEnd gives an end a first STUN
// server that does not answer, as the first relay of a citizen that is
// down, and the STUN server of a second relay. The end still gets its
// public address, after the gather timeout at most.
func TestADeadFirstSTUNServerDoesNotStopAnEnd(t *testing.T) {
	a, _, _ := natPair(t)
	pc, err := NewConnection(Options{STUN: "stun:1.2.3.99:3478", MoreSTUN: []string{"stun:1.2.3.4:3478"}, Net: a})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pc.Close() }()
	if _, err := pc.CreateDataChannel("probe", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err == nil {
		offer, err = Describe(pc, offer)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(offer.SDP, "typ srflx raddr") || !strings.Contains(offer.SDP, "1.2.3.10") {
		t.Fatalf("an end with a dead first STUN server got no public address from the second:\n%s", offer.SDP)
	}
}

// An end that has only TURN over TCP, as one with no UDP out of its
// network, still gets an address of type relay: the network types of an
// end limit its candidates, not how it reaches the TURN server.
func TestTURNOverTCPGivesARelayAddress(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tcp, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server, err := turn.Serve(conn, turn.Config{Secret: "secret", PublicIP: net.IPv4(127, 0, 0, 1), RelayHost: "127.0.0.1", MinPort: 53000, MaxPort: 53099, Quota: 1 << 20, TCP: tcp})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	credential, err := server.Credential(strings.Repeat("ab", 32), "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	pc, err := NewConnection(Options{TURN: &webrtc.ICEServer{
		URLs:       []string{"turn:" + tcp.Addr().String() + "?transport=tcp"},
		Username:   credential.Username,
		Credential: credential.Password,
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	if _, err := pc.CreateDataChannel("file", nil); err != nil {
		t.Fatal(err)
	}
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	local, err := Describe(pc, offer)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(local.SDP, "typ relay") {
		t.Fatalf("no relay address through TURN over TCP:\n%s", local.SDP)
	}
}
