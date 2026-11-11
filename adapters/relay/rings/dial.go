package rings

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"syscall"
)

// newClient makes the HTTP client of the rings. It follows no redirect,
// uses no proxy, and dials only an address that passed the checks.
func (r *Rings) newClient() *http.Client {
	transport := &http.Transport{
		Proxy:               nil,
		DialContext:         r.dial,
		TLSClientConfig:     r.cfg.TLS,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: callTimeout,
		ForceAttemptHTTP2:   true,
	}
	return &http.Client{
		Transport:     transport,
		Timeout:       callTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dial resolves the host again, refuses it if any of its addresses is not
// public, and connects to a checked address.
func (r *Rings) dial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addrs, err := r.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	var last error
	for _, addr := range addrs {
		target := net.JoinHostPort(addr.String(), port)
		var conn net.Conn
		if r.cfg.Dial != nil {
			conn, err = r.cfg.Dial(ctx, network, target)
		} else {
			dialer := net.Dialer{Timeout: callTimeout, Control: r.control}
			conn, err = dialer.DialContext(ctx, network, target)
		}
		if err == nil {
			return conn, nil
		}
		last = err
	}
	return nil, last
}

// control checks the address that a connection goes to.
func (r *Rings) control(_, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	if !r.public(ap.Addr()) {
		return fmt.Errorf("%s is not a public address", ap.Addr())
	}
	return nil
}

// resolve returns the addresses of host, or an error if one is not public.
func (r *Rings) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		if !r.public(addr) {
			return nil, fmt.Errorf("%s is not a public address", addr)
		}
		return []netip.Addr{addr}, nil
	}
	addrs, err := r.cfg.Lookup(ctx, host)
	if err != nil {
		return nil, fmt.Errorf("the host %s does not resolve", host)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("the host %s has no address", host)
	}
	for _, addr := range addrs {
		if !r.public(addr) {
			return nil, fmt.Errorf("the host %s resolves to %s, which is not a public address", host, addr.Unmap())
		}
	}
	return addrs, nil
}

// notPublic are prefixes that netip does not mark, and that do not reach
// a public server: this network, shared address space, protocol
// assignments, benchmarking, reserved and broadcast, NAT64 and 6to4.
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("2002::/16"),
}

// public says whether a ring can go to addr.
func (r *Rings) public(addr netip.Addr) bool {
	addr = addr.Unmap()
	if !addr.IsValid() || addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() || addr.IsUnspecified() {
		return false
	}
	if slices.ContainsFunc(notPublic, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return false
	}
	return !r.own[addr]
}

// ownAddrs are the addresses of the relay: those of its interfaces, and
// the public addresses that the operator named.
func ownAddrs(named []netip.Addr) map[netip.Addr]bool {
	own := map[netip.Addr]bool{}
	for _, addr := range named {
		own[addr.Unmap()] = true
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if prefix, err := netip.ParsePrefix(a.String()); err == nil {
			own[prefix.Addr().Unmap()] = true
		}
	}
	return own
}
