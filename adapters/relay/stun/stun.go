// Package stun answers STUN binding requests (RFC 8489) for a relay. A
// citizen behind a NAT asks it for its public address, so that two
// citizens can open a direct path with no STUN server of a third party.
package stun

import (
	"errors"
	"net"

	"github.com/pion/stun/v4"
)

// Port is the STUN port of a relay. A citizen asks the host of its relay
// on this port.
const Port = 3478

// Serve answers each binding request on conn with the address and port
// that the request came from. It ignores all other packets. It returns
// when conn closes.
func Serve(conn net.PacketConn) error {
	buf := make([]byte, 1500)
	for {
		n, from, err := conn.ReadFrom(buf)
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return err
		}
		reply, ok := answer(buf[:n], from)
		if !ok {
			continue
		}
		// A reply that does not go is the same as a lost packet: the
		// client asks again.
		_, _ = conn.WriteTo(reply, from)
	}
}

// answer makes the reply to one packet, or returns false if the packet is
// not a binding request.
func answer(packet []byte, from net.Addr) ([]byte, bool) {
	addr, ok := from.(*net.UDPAddr)
	if !ok || !stun.IsMessage(packet) {
		return nil, false
	}
	request := &stun.Message{Raw: append([]byte(nil), packet...)}
	if request.Decode() != nil || request.Type != stun.BindingRequest {
		return nil, false
	}
	ip := addr.IP
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	reply, err := stun.Build(
		stun.NewTransactionIDSetter(request.TransactionID),
		stun.BindingSuccess,
		&stun.XORMappedAddress{IP: ip, Port: addr.Port},
		stun.Fingerprint,
	)
	if err != nil {
		return nil, false
	}
	return reply.Raw, true
}
