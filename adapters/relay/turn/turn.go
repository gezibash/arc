// Package turn is the TURN server of a relay (RFC 8656). Two citizens whose
// NATs block a direct path send their packets through it, as the last
// resort. It also answers STUN binding requests on the same port.
//
// A citizen gets a credential with a NIP-98 request to GET /turn on the
// relay. The credential holds the key of the citizen, so the relay counts
// the relayed bytes of each key against a quota that the operator sets.
package turn

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/logging"
	"github.com/pion/transport/v5"
	pturn "github.com/pion/turn/v5"
)

// Path is the HTTP path that gives a credential.
const Path = "/turn"

// Realm is the TURN realm of a relay.
const Realm = "arc"

const (
	// DefaultTTL is the life of a credential. A transfer through the relay
	// must end in this time, because a refresh with an old credential fails.
	DefaultTTL = time.Hour
	// DefaultPeriod is the time after which each quota starts again.
	DefaultPeriod = 24 * time.Hour
)

// errQuota is the error of a relay socket after the citizen used its quota.
var errQuota = errors.New("turn: the quota of the citizen is used")

// Config is the setting of a TURN server.
type Config struct {
	// Secret signs the credentials. It must not be empty.
	Secret string
	// PublicIP is the address that the relay sockets have for the peers.
	PublicIP net.IP
	// RelayHost is the address that the relay sockets listen on, such as
	// "0.0.0.0", or "fly-global-services" on Fly.io.
	RelayHost string
	// MinPort and MaxPort are the range of the relay sockets. Each
	// allocation takes one port.
	MinPort, MaxPort uint16
	// Quota is the number of relayed bytes that one citizen can use in
	// each period, in both directions together.
	Quota int64
	// Period is the time after which each quota starts again. Zero gives
	// DefaultPeriod.
	Period time.Duration
	// TTL is the life of a credential. Zero gives DefaultTTL.
	TTL time.Duration
	// Net replaces the network of the machine. Tests use it.
	Net transport.Net
	// TCP, if not nil, also serves TURN over TCP, such as behind a TLS
	// proxy on port 443 for citizens that cannot send UDP. The relay
	// sockets stay UDP.
	TCP net.Listener
	// URLs are more addresses that each credential names, after the UDP
	// one, such as "turns:relay.example:443?transport=tcp".
	URLs []string
}

// Credential is the reply to GET /turn.
type Credential struct {
	URLs     []string `json:"urls"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	// TTL is the life of the credential, in seconds.
	TTL int `json:"ttl"`
	// QuotaLeft is the number of bytes that the citizen can still relay in
	// this period.
	QuotaLeft int64 `json:"quota_left"`
}

// Server is a running TURN server.
type Server struct {
	config Config
	port   int
	ledger *ledger
	turn   *pturn.Server
}

// Serve starts a TURN server on conn. conn is the UDP socket of the relay
// that citizens know, port 3478 by default. Close the server to stop it.
func Serve(conn net.PacketConn, config Config) (*Server, error) {
	if config.Secret == "" {
		return nil, errors.New("turn: no secret")
	}
	if config.PublicIP == nil {
		return nil, errors.New("turn: no public IP address")
	}
	if config.MinPort == 0 || config.MaxPort < config.MinPort {
		return nil, fmt.Errorf("turn: bad port range %d-%d", config.MinPort, config.MaxPort)
	}
	if config.Period == 0 {
		config.Period = DefaultPeriod
	}
	if config.TTL == 0 {
		config.TTL = DefaultTTL
	}
	port := 0
	if addr, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		port = addr.Port
	}
	s := &Server{config: config, port: port, ledger: newLedger(config.Quota, config.Period)}
	quiet := logging.NewDefaultLoggerFactory()
	quiet.DefaultLogLevel = logging.LogLevelDisabled
	generator := &meteredGenerator{
		RelayAddressGeneratorPortRange: &pturn.RelayAddressGeneratorPortRange{
			RelayAddress: config.PublicIP,
			Address:      config.RelayHost,
			MinPort:      config.MinPort,
			MaxPort:      config.MaxPort,
			Net:          config.Net,
		},
		ledger:  s.ledger,
		localIP: localIP(config),
	}
	var listeners []pturn.ListenerConfig
	if config.TCP != nil {
		listeners = []pturn.ListenerConfig{{Listener: config.TCP, RelayAddressGenerator: generator}}
	}
	server, err := pturn.NewServer(pturn.ServerConfig{
		Realm:         Realm,
		LoggerFactory: quiet,
		AuthHandler:   pturn.LongTermTURNRESTAuthHandler(config.Secret, quiet.NewLogger("turn")),
		QuotaHandler: func(username, _ string, _ net.Addr) bool {
			return s.ledger.left(userOf(username)) > 0
		},
		PacketConnConfigs: []pturn.PacketConnConfig{{
			PacketConn:            conn,
			RelayAddressGenerator: generator,
		}},
		ListenerConfigs: listeners,
	})
	if err != nil {
		return nil, err
	}
	s.turn = server
	return s, nil
}

// localIP is the address that the relay sockets listen on, if it is not
// the public address. On Fly.io, the network drops a packet from the relay
// to its own public address. So a relay socket sends a packet for another
// relay socket to this address, and the host delivers it (#264).
func localIP(config Config) net.IP {
	addr, err := net.ResolveIPAddr("ip4", config.RelayHost)
	if err != nil || addr.IP == nil || addr.IP.IsUnspecified() || addr.IP.Equal(config.PublicIP) {
		return nil
	}
	return addr.IP
}

// Close stops the server and its allocations.
func (s *Server) Close() error { return s.turn.Close() }

// Left is the number of bytes that a citizen can still relay in this
// period. key is the public key as hex.
func (s *Server) Left(key string) int64 { return s.ledger.left(key) }

// Credential makes a credential for a citizen. host is the host name of the
// relay, as the citizen knows it.
func (s *Server) Credential(key, host string) (Credential, error) {
	username, password, err := pturn.GenerateLongTermTURNRESTCredentials(s.config.Secret, key, s.config.TTL)
	if err != nil {
		return Credential{}, err
	}
	return Credential{
		URLs:      append([]string{"turn:" + net.JoinHostPort(host, strconv.Itoa(s.port)) + "?transport=udp"}, s.config.URLs...),
		Username:  username,
		Password:  password,
		TTL:       int(s.config.TTL / time.Second),
		QuotaLeft: s.ledger.left(key),
	}, nil
}

// ServeHTTP answers GET /turn. It gives a credential to the citizen that
// signed the NIP-98 authorization of the request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	key, err := VerifyAuth(r.Header.Get("Authorization"), r.Method, r.Host, r.URL.Path, time.Now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnauthorized)
		return
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	credential, err := s.Credential(key.Hex(), host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(credential)
}

// userOf is the key in a username of the form <expiry>:<key>.
func userOf(username string) string {
	if _, key, ok := strings.Cut(username, ":"); ok {
		return key
	}
	return username
}

// ledger counts the relayed bytes of each key in the current period.
type ledger struct {
	quota  int64
	period time.Duration
	now    func() time.Time

	mu    sync.Mutex
	start time.Time
	used  map[string]int64
}

func newLedger(quota int64, period time.Duration) *ledger {
	return &ledger{quota: quota, period: period, now: time.Now, used: map[string]int64{}}
}

// roll starts a new period if the current one ended. Hold mu.
func (l *ledger) roll() {
	if now := l.now(); now.Sub(l.start) >= l.period {
		l.start, l.used = now, map[string]int64{}
	}
}

func (l *ledger) left(key string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	return max(l.quota-l.used[key], 0)
}

// spend adds n bytes to the count of key. It says false if the count is
// then above the quota.
func (l *ledger) spend(key string, n int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.roll()
	l.used[key] += int64(n)
	return l.used[key] <= l.quota
}

// meteredGenerator makes relay sockets that count their bytes against the
// quota of the citizen of the allocation.
type meteredGenerator struct {
	*pturn.RelayAddressGeneratorPortRange
	ledger *ledger
	// localIP is the address that the relay sockets listen on, if it is
	// not the public address and not unspecified.
	localIP net.IP
}

func (g *meteredGenerator) AllocatePacketConn(config pturn.AllocateListenerConfig) (net.PacketConn, net.Addr, error) {
	conn, addr, err := g.RelayAddressGeneratorPortRange.AllocatePacketConn(config)
	if err != nil {
		return nil, nil, err
	}
	c := &meteredConn{PacketConn: conn, key: config.UserID, ledger: g.ledger}
	if g.localIP != nil {
		c.publicIP, c.localIP = g.RelayAddress, g.localIP
		c.minPort, c.maxPort = int(g.MinPort), int(g.MaxPort)
	}
	return c, addr, nil
}

// meteredConn is a relay socket. When the citizen used its quota, the
// socket closes, and the allocation relays nothing more.
type meteredConn struct {
	net.PacketConn
	key    string
	ledger *ledger
	// publicIP and localIP are set if the socket listens on localIP behind
	// publicIP. A peer that is another relay socket then has localIP on
	// the host and publicIP for the clients. Only the ports of the relay
	// sockets change, so a client cannot reach another service of the host.
	publicIP, localIP net.IP
	minPort, maxPort  int
}

func (c *meteredConn) ReadFrom(b []byte) (int, net.Addr, error) {
	n, addr, err := c.PacketConn.ReadFrom(b)
	if n > 0 && !c.ledger.spend(c.key, n) {
		_ = c.Close()
		return 0, nil, errQuota
	}
	return n, c.swapIP(addr, c.localIP, c.publicIP), err
}

func (c *meteredConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if !c.ledger.spend(c.key, len(b)) {
		_ = c.Close()
		return 0, errQuota
	}
	return c.PacketConn.WriteTo(b, c.swapIP(addr, c.publicIP, c.localIP))
}

// swapIP gives addr with the IP address to in place of from. It gives
// addr unchanged if its IP address is not from, or its port is not the
// port of a relay socket.
func (c *meteredConn) swapIP(addr net.Addr, from, to net.IP) net.Addr {
	if u, ok := addr.(*net.UDPAddr); ok && from != nil && u.IP.Equal(from) && u.Port >= c.minPort && u.Port <= c.maxPort {
		return &net.UDPAddr{IP: to, Port: u.Port}
	}
	return addr
}
