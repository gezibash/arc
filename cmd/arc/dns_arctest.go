//go:build arctest

package main

import (
	"context"
	"net"
	"os"
)

// In a build with the tag arctest, ARC_TEST_DNS names a DNS server, as
// host:port, that answers each lookup of arc. A test of the delivery spec
// gives it a server that says a name does not exist, or one that never
// answers, so the test does not depend on the DNS of the machine.
func init() {
	server := os.Getenv("ARC_TEST_DNS")
	if server == "" {
		return
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", server)
		},
	}
}
