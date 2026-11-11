// Command arc-relay runs an ARC relay: a Nostr relay that serves sealed
// data only to its author, with write limits, and optional STUN, TURN and
// rings. It holds no citizen key. See docs/delivery/SPEC.md.
//
//	arc-relay [--listen <address>] [--db <file>] [flags]
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/relayserver"
	"github.com/gezibash/arc/adapters/relay/rings"
	"github.com/gezibash/arc/adapters/relay/stun"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := relayCommand().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "arc-relay: "+terminal.EscapeLine(err.Error()))
		os.Exit(1)
	}
}

// defaultDB is the store of a relay with no --db: relay.db in ARC_HOME, or
// in ~/.arc. That is where arc relay serve kept it. An older arc kept it in
// ~/.config/arc: when only that store exists, the relay does not start on
// a new, empty one, and says which --db to give.
func defaultDB() (string, error) {
	if dir := os.Getenv("ARC_HOME"); dir != "" {
		return filepath.Join(dir, "relay.db"), nil
	}
	user, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(user, ".arc", "relay.db")
	older := filepath.Join(user, ".config", "arc", "relay.db")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(older); err == nil {
			return "", fmt.Errorf("the store of this relay is %s, of an older arc; run arc-relay --db %s", older, older)
		}
	}
	return path, nil
}

func relayCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "arc-relay", Short: "Run an ARC relay on this machine", Args: cobra.NoArgs,
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE:          runRelay,
	}
	command.Flags().String("listen", "127.0.0.1:7447", "the address to listen on")
	command.Flags().String("db", "", "the file that holds the events (default relay.db in ARC_HOME, or in ~/.arc)")
	command.Flags().Int("max-event-bytes", 0, "refuse an event larger than this, as JSON (0: no cap on ephemeral events; a kept event has at most the protocol limit of 65,536 bytes, and a gift wrap the mail limit)")
	command.Flags().Bool("wrap-auth", false, "take a gift wrap only after NIP-42 authentication, or with proof of work")
	command.Flags().Int("wrap-pow", 0, "the NIP-13 difficulty that lets a gift wrap in without authentication (0: none)")
	command.Flags().Int("rate", 0, "the events that one IP address can write each minute (0: no limit)")
	command.Flags().Int("burst", 0, "the events that one IP address can write at once (default the rate)")
	command.Flags().String("ip-header", "", "the HTTP header that holds the client IP address, when a proxy sets it")
	command.Flags().Int64("max-store-mb", 0, "refuse new stored events when the store uses this many MiB (0: no cap)")
	command.Flags().String("stun", "", "the UDP address that answers STUN, or none (default: the host of --listen, port 3478)")
	command.Flags().String("turn-ports", "", "serve TURN on the STUN port, with relay sockets on this UDP port range, such as 49152-49251 (needs TURN_SECRET)")
	command.Flags().String("turn-ip", "", "the public IP address of the TURN relay sockets (default: the host of --listen)")
	command.Flags().Int64("turn-quota", 1024, "the MiB that one citizen can relay through TURN in 24 hours (0: refuse every allocation)")
	command.Flags().String("turn-tcp", "", "also serve TURN over TCP on this address, such as 127.0.0.1:3479 behind a TLS proxy on port 443 (needs --turn-ports)")
	command.Flags().StringArray("turn-url", nil, "another address that TURN credentials name, such as turns:relay.example:443?transport=tcp (repeatable; needs --turn-ports)")
	command.Flags().Bool("rings", false, "ring a registered citizen at its URL when mail waits unread (needs RINGS_SECRET)")
	command.Flags().Int("rings-daily-cap", rings.DefaultDailyCap, "the most rings that a citizen can ask for in one UTC day")
	command.Flags().Int("rings-per-minute", rings.DefaultPerMinute, "the most rings of the whole relay in one minute")
	command.Flags().IntSlice("rings-ports", []int{443}, "the ports that a ring URL can name")
	return command
}

// runRelay runs the relay until a signal stops it: the store, the
// WebSocket listener, STUN and TURN, the rings, and the HTTP server.
func runRelay(command *cobra.Command, _ []string) error {
	listen, _ := command.Flags().GetString("listen")
	path, _ := command.Flags().GetString("db")
	if path == "" {
		var err error
		if path, err = defaultDB(); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ring, err := ringSetting(command)
	if err != nil {
		return err
	}
	rl, connections, ringer, closeDB, err := relayserver.New(path, writePolicy(command), ring, logger)
	if err != nil {
		return err
	}
	defer closeDB()

	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	listener = connections.Listen(listener)
	fmt.Fprintf(command.OutOrStdout(), "relay listens on ws://%s\n", listener.Addr())
	turnServer, stopSTUN, err := startSTUN(command, listen, logger)
	if err != nil {
		return err
	}
	defer stopSTUN()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if ringer != nil {
		go ringer.Run(ctx)
		fmt.Fprintf(command.OutOrStdout(), "relay rings citizens, at most %d rings each minute\n", ring.PerMinute)
	}
	server := &http.Server{Handler: relayserver.Handler(rl, turnServer, ringer), ConnContext: connections.ConnContext}
	go func() { <-ctx.Done(); _ = server.Close() }()
	go connections.Run(ctx, time.Minute)
	if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// startSTUN opens the STUN port of the relay, and serves TURN on it when
// the flags ask for TURN. It returns the TURN server, or nil, and a
// function that closes what it opened.
func startSTUN(command *cobra.Command, listen string, logger *slog.Logger) (*turn.Server, func(), error) {
	none := func() {}
	turnConfig, err := turnSetting(command)
	if err != nil {
		return nil, none, err
	}
	stunAddress := stunListen(command, listen)
	if stunAddress == "" {
		if turnConfig != nil {
			return nil, none, errors.New("TURN needs the STUN port; do not set --stun none with --turn-ports")
		}
		return nil, none, nil
	}
	// A second relay on one machine cannot take the default
	// port. It runs with no STUN server. A port that the
	// operator sets must open.
	conn, err := net.ListenPacket("udp", stunAddress)
	switch {
	case err != nil && (command.Flags().Changed("stun") || turnConfig != nil):
		return nil, none, fmt.Errorf("STUN: %w", err)
	case err != nil:
		fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("relay answers no STUN: %v (set --stun)", err)))
		return nil, none, nil
	case turnConfig != nil:
		// The TURN server answers STUN binding requests too.
		host, _, _ := net.SplitHostPort(stunAddress)
		turnConfig.RelayHost = host
		turnConfig.Logger = logger
		if address, _ := command.Flags().GetString("turn-tcp"); address != "" {
			if turnConfig.TCP, err = net.Listen("tcp", address); err != nil {
				_ = conn.Close()
				return nil, none, fmt.Errorf("TURN over TCP: %w", err)
			}
		}
		turnServer, err := turn.Serve(conn, *turnConfig)
		if err != nil {
			_ = conn.Close()
			if turnConfig.TCP != nil {
				_ = turnConfig.TCP.Close()
			}
			return nil, none, fmt.Errorf("TURN: %w", err)
		}
		fmt.Fprintf(command.OutOrStdout(), "relay answers STUN and TURN on udp://%s, relay ports %d-%d on %s\n", conn.LocalAddr(), turnConfig.MinPort, turnConfig.MaxPort, turnConfig.PublicIP)
		if turnConfig.TCP != nil {
			fmt.Fprintf(command.OutOrStdout(), "relay answers TURN on tcp://%s\n", turnConfig.TCP.Addr())
		}
		return turnServer, func() { _ = turnServer.Close() }, nil
	default:
		go func() {
			if err := stun.Serve(conn); err != nil {
				fmt.Fprintln(os.Stderr, "the STUN server stopped:", terminal.EscapeLine(err.Error()))
			}
		}()
		fmt.Fprintf(command.OutOrStdout(), "relay answers STUN on udp://%s\n", conn.LocalAddr())
		return nil, func() { _ = conn.Close() }, nil
	}
}

// ringSetting reads the ring flags of a relay. It returns nil if the relay
// offers no rings. The secret that encrypts the ring keys comes from the
// environment variable RINGS_SECRET, so it is not in the process list.
func ringSetting(command *cobra.Command) (*rings.Config, error) {
	flags := command.Flags()
	if on, _ := flags.GetBool("rings"); !on {
		return nil, nil
	}
	secret := os.Getenv("RINGS_SECRET")
	if secret == "" {
		return nil, errors.New("--rings needs the environment variable RINGS_SECRET")
	}
	var config rings.Config
	config.Secret = secret
	config.DailyCap, _ = flags.GetInt("rings-daily-cap")
	config.PerMinute, _ = flags.GetInt("rings-per-minute")
	config.Ports, _ = flags.GetIntSlice("rings-ports")
	if config.DailyCap < 1 || config.PerMinute < 1 {
		return nil, errors.New("--rings-daily-cap and --rings-per-minute must be at least 1")
	}
	if address, _ := flags.GetString("turn-ip"); address != "" {
		if ip, err := netip.ParseAddr(address); err == nil {
			config.Own = append(config.Own, ip)
		}
	}
	return &config, nil
}

// turnSetting reads the TURN flags of a relay. It returns nil if the relay
// serves no TURN. The secret comes from the environment variable
// TURN_SECRET, so it is not in the process list.
func turnSetting(command *cobra.Command) (*turn.Config, error) {
	flags := command.Flags()
	ports, _ := flags.GetString("turn-ports")
	if ports == "" {
		if flags.Changed("turn-tcp") || flags.Changed("turn-url") {
			return nil, errors.New("--turn-tcp and --turn-url need --turn-ports")
		}
		return nil, nil
	}
	low, high, ok := strings.Cut(ports, "-")
	minPort, err1 := strconv.ParseUint(low, 10, 16)
	maxPort, err2 := strconv.ParseUint(high, 10, 16)
	if !ok || err1 != nil || err2 != nil || minPort == 0 || maxPort < minPort {
		return nil, fmt.Errorf("--turn-ports %q is not a range such as 49152-49251", ports)
	}
	secret := os.Getenv("TURN_SECRET")
	if secret == "" {
		return nil, errors.New("--turn-ports needs the environment variable TURN_SECRET")
	}
	address, _ := flags.GetString("turn-ip")
	if address == "" {
		listen, _ := flags.GetString("listen")
		address, _, _ = net.SplitHostPort(listen)
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.IsUnspecified() {
		return nil, fmt.Errorf("TURN needs the public IP address of this machine: set --turn-ip (not %q)", address)
	}
	quota, _ := flags.GetInt64("turn-quota")
	urls, _ := flags.GetStringArray("turn-url")
	for _, u := range urls {
		if !strings.HasPrefix(u, "turn:") && !strings.HasPrefix(u, "turns:") {
			return nil, fmt.Errorf("--turn-url %q is not a turn: or turns: URL", u)
		}
	}
	return &turn.Config{Secret: secret, PublicIP: ip, MinPort: uint16(minPort), MaxPort: uint16(maxPort), Quota: quota << 20, URLs: urls}, nil
}

// stunListen is the UDP address of the STUN server of a relay, or empty for
// none. A citizen asks the host of its relay on port 3478, so the default
// keeps the host of the relay.
func stunListen(command *cobra.Command, listen string) string {
	value, _ := command.Flags().GetString("stun")
	switch value {
	case "none":
		return ""
	case "":
		host, _, _ := net.SplitHostPort(listen)
		return net.JoinHostPort(host, strconv.Itoa(stun.Port))
	}
	return value
}

// writePolicy reads the write limits of a relay from its flags.
func writePolicy(command *cobra.Command) limits.Policy {
	flags := command.Flags()
	var p limits.Policy
	p.MaxEventBytes, _ = flags.GetInt("max-event-bytes")
	p.WrapAuth, _ = flags.GetBool("wrap-auth")
	p.WrapPoW, _ = flags.GetInt("wrap-pow")
	p.Rate, _ = flags.GetInt("rate")
	p.Burst, _ = flags.GetInt("burst")
	p.IPHeader, _ = flags.GetString("ip-header")
	mb, _ := flags.GetInt64("max-store-mb")
	p.MaxStoreBytes = mb << 20
	return p
}
