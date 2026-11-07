package relay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr/nip11"
)

// stallLimit bounds the first attempt to reach a relay: the TCP connect, the
// TLS handshake and the WebSocket handshake. A network can lose the packets
// of one address family only. Go's dialer starts IPv4 when an IPv6 connect
// gets no answer after 300 ms, but a connection that opens and then stalls
// waits for the whole deadline. After stallLimit, the second attempt uses
// the other family.
var stallLimit = 4 * time.Second

// ErrStalled is the error of a first attempt that did not end within
// stallLimit: the relay gave no answer.
var ErrStalled = errors.New("no answer within the stall limit")

// families holds the relay hosts that IPv4 reached, as "tcp4". It lasts as
// long as the process. A host that IPv6 reached gets no entry: a dial that
// names only "tcp6" loses the IPv4 race of Go's dialer, and a network that
// drops IPv6 SYNs then waits a second or more for each retransmit.
var families = struct {
	sync.Mutex
	of map[string]string
}{of: map[string]string{}}

// family returns the family that worked last for host, or "tcp" for both.
func family(host string) string {
	families.Lock()
	defer families.Unlock()
	if known, ok := families.of[host]; ok {
		return known
	}
	return "tcp"
}

// remember keeps the family that worked for host, if it is IPv4.
func remember(host, family string) {
	families.Lock()
	defer families.Unlock()
	if family == "tcp4" {
		families.of[host] = family
	} else {
		delete(families.of, host)
	}
}

// dialer is the dialer of each attempt.
var dialer = &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}

// reach runs try with an HTTP client that reaches the host of rawURL. The
// first attempt uses IPv4 if IPv4 worked for the host before. Otherwise it
// uses both families as Go's dialer races them. If that attempt fails, or does not end
// within stallLimit, reach runs try once more over the other family.
func reach(ctx context.Context, rawURL string, try func(context.Context, *http.Client) error) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	host := u.Hostname()

	network := family(host)
	first, cancel := context.WithTimeoutCause(ctx, stallLimit, ErrStalled)
	used, err := attempt(first, network, try)
	if err != nil && errors.Is(context.Cause(first), ErrStalled) {
		err = fmt.Errorf("%w: %w", ErrStalled, err)
	}
	cancel()
	if err == nil {
		remember(host, used)
		return nil
	}
	if ctx.Err() != nil {
		return err
	}
	// A first attempt that raced both families, and reached neither within
	// stallLimit, tried each address already: a host behind a black hole.
	// Another attempt would wait the rest of the timeout (#439).
	if used == "" && network == "tcp" && errors.Is(err, ErrStalled) {
		return err
	}

	other := "tcp4"
	switch {
	case used == "tcp4", used == "" && network == "tcp4":
		other = "tcp6"
	}
	used, again := attempt(ctx, other, try)
	if again != nil {
		return fmt.Errorf("%w; again over %s: %w", err, other, again)
	}
	remember(host, used)
	return nil
}

// attempt runs try with a client that dials over network. It returns the
// family of the last connection that the client opened, or "" if it opened
// none.
func attempt(ctx context.Context, network string, try func(context.Context, *http.Client) error) (string, error) {
	var mu sync.Mutex
	used := ""
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			conn, err := dialer.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			family := "tcp4"
			if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok && a.IP.To4() == nil {
				family = "tcp6"
			}
			mu.Lock()
			used = family
			mu.Unlock()
			return conn, nil
		},
	}
	// A WebSocket owns its connection after the handshake. Only a NIP-11
	// request leaves an idle connection behind.
	defer transport.CloseIdleConnections()
	err := try(ctx, &http.Client{Transport: transport})
	mu.Lock()
	defer mu.Unlock()
	return used, err
}

// information fetches the NIP-11 document of a relay.
func information(ctx context.Context, rawURL string) (nip11.RelayInformationDocument, error) {
	var info nip11.RelayInformationDocument
	address := "http" + strings.TrimPrefix(rawURL, "ws")
	err := reach(ctx, rawURL, func(ctx context.Context, client *http.Client) error {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err != nil {
			return err
		}
		request.Header.Set("Accept", "application/nostr+json")
		response, err := client.Do(request)
		if err != nil {
			return err
		}
		defer func() { _ = response.Body.Close() }()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("information document: %s", response.Status)
		}
		info = nip11.RelayInformationDocument{}
		return json.NewDecoder(response.Body).Decode(&info)
	})
	return info, err
}
