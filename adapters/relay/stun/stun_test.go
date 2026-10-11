package stun

import (
	"net"
	"testing"
	"time"

	"github.com/pion/stun/v4"
)

// TestBindingRequest asks the server for the address of a client socket.
// The expected address is the address of that socket, from the kernel.
func TestBindingRequest(t *testing.T) {
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- Serve(conn) }()
	defer func() {
		_ = conn.Close()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()

	client, err := net.DialUDP("udp4", nil, conn.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// A packet that is not STUN, and a STUN packet that is not a binding
	// request, get no reply, and the server answers the next request.
	if _, err := client.Write([]byte("not stun")); err != nil {
		t.Fatal(err)
	}
	indication := stun.MustBuild(stun.TransactionID, stun.NewType(stun.MethodBinding, stun.ClassIndication))
	if _, err := client.Write(indication.Raw); err != nil {
		t.Fatal(err)
	}
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
	if reply.Type != stun.BindingSuccess || reply.TransactionID != request.TransactionID {
		t.Fatalf("reply is %v with transaction %x, want %v with %x", reply.Type, reply.TransactionID, stun.BindingSuccess, request.TransactionID)
	}
	if err := stun.Fingerprint.Check(reply); err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	var mapped stun.XORMappedAddress
	if err := mapped.GetFrom(reply); err != nil {
		t.Fatal(err)
	}
	local := client.LocalAddr().(*net.UDPAddr)
	if !mapped.IP.Equal(local.IP) || mapped.Port != local.Port || len(mapped.IP) != net.IPv4len {
		t.Fatalf("mapped address %s (%d bytes), want %s (4 bytes)", mapped, len(mapped.IP), local)
	}
}
