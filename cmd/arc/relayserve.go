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
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"fiatjaf.com/nostr/eventstore/boltdb"
	"fiatjaf.com/nostr/khatru"
	"github.com/gezibash/arc/adapters/relay/limits"
	"github.com/gezibash/arc/adapters/relay/relaylog"
	"github.com/gezibash/arc/adapters/relay/rings"
	"github.com/gezibash/arc/adapters/relay/sealed"
	"github.com/gezibash/arc/adapters/relay/stun"
	"github.com/gezibash/arc/adapters/relay/turn"
	"github.com/gezibash/arc/sdk/terminal"
	"github.com/spf13/cobra"
)

// newRelay makes the relay of arc relay serve, with its store in the file at
// path. The relay logs its connections to logger. If ring is not nil, the
// relay offers rings. Call the returned function to close the store.
func newRelay(path string, p limits.Policy, ring *rings.Config, logger *slog.Logger) (*khatru.Relay, *relaylog.Log, *rings.Rings, func(), error) {
	db := &boltdb.BoltBackend{Path: path}
	if err := db.Init(); err != nil {
		return nil, nil, nil, nil, err
	}
	connections := relaylog.New(logger)
	p.Refused = connections.Refused
	rl := khatru.NewRelay()
	rl.UseEventstore(db, 500)
	rl.Negentropy = true
	// khatru copies Info for each NIP-11 request and appends to the copy;
	// with no spare room, each append copies, and requests do not race (#425).
	rl.Info.SupportedNIPs = slices.Clip(append(rl.Info.SupportedNIPs, 77))
	readers := sealed.Protect(rl)
	limits.Apply(rl, db.DB, p)
	connections.Watch(rl)
	var ringer *rings.Rings
	if ring != nil {
		config := *ring
		config.Readers = readers
		config.Logger = logger
		var err error
		if ringer, err = rings.Apply(rl, db.DB, config); err != nil {
			db.Close()
			return nil, nil, nil, nil, err
		}
	}
	return rl, connections, ringer, db.Close, nil
}

func serveCommand() *cobra.Command {
	command := &cobra.Command{
		Use: "serve", Short: "Run a relay on this machine", Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			listen, _ := command.Flags().GetString("listen")
			path, _ := command.Flags().GetString("db")
			if path == "" {
				dir, err := rootDir(command)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(dir, 0o700); err != nil {
					return err
				}
				path = filepath.Join(dir, "relay.db")
			}

			logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
			ring, err := ringSetting(command)
			if err != nil {
				return err
			}
			rl, connections, ringer, closeDB, err := newRelay(path, writePolicy(command), ring, logger)
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
			turnConfig, err := turnSetting(command)
			if err != nil {
				return err
			}
			var turnServer *turn.Server
			if stunAddress := stunListen(command, listen); stunAddress != "" {
				// A second relay on one machine cannot take the default
				// port. It runs with no STUN server. A port that the
				// operator sets must open.
				conn, err := net.ListenPacket("udp", stunAddress)
				switch {
				case err != nil && (command.Flags().Changed("stun") || turnConfig != nil):
					return fmt.Errorf("STUN: %w", err)
				case err != nil:
					fmt.Fprintln(os.Stderr, terminal.EscapeLine(fmt.Sprintf("relay answers no STUN: %v (set --stun)", err)))
				case turnConfig != nil:
					// The TURN server answers STUN binding requests too.
					host, _, _ := net.SplitHostPort(stunAddress)
					turnConfig.RelayHost = host
					turnConfig.Logger = logger
					if address, _ := command.Flags().GetString("turn-tcp"); address != "" {
						if turnConfig.TCP, err = net.Listen("tcp", address); err != nil {
							_ = conn.Close()
							return fmt.Errorf("TURN over TCP: %w", err)
						}
					}
					if turnServer, err = turn.Serve(conn, *turnConfig); err != nil {
						_ = conn.Close()
						if turnConfig.TCP != nil {
							_ = turnConfig.TCP.Close()
						}
						return fmt.Errorf("TURN: %w", err)
					}
					defer func() { _ = turnServer.Close() }()
					fmt.Fprintf(command.OutOrStdout(), "relay answers STUN and TURN on udp://%s, relay ports %d-%d on %s\n", conn.LocalAddr(), turnConfig.MinPort, turnConfig.MaxPort, turnConfig.PublicIP)
					if turnConfig.TCP != nil {
						fmt.Fprintf(command.OutOrStdout(), "relay answers TURN on tcp://%s\n", turnConfig.TCP.Addr())
					}
				default:
					defer func() { _ = conn.Close() }()
					go func() {
						if err := stun.Serve(conn); err != nil {
							fmt.Fprintln(os.Stderr, "the STUN server stopped:", terminal.EscapeLine(err.Error()))
						}
					}()
					fmt.Fprintf(command.OutOrStdout(), "relay answers STUN on udp://%s\n", conn.LocalAddr())
				}
			} else if turnConfig != nil {
				return errors.New("TURN needs the STUN port; do not set --stun none with --turn-ports")
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			if ringer != nil {
				go ringer.Run(ctx)
				fmt.Fprintf(command.OutOrStdout(), "relay rings citizens, at most %d rings each minute\n", ring.PerMinute)
			}
			server := &http.Server{Handler: relayHandler(rl, turnServer, ringer), ConnContext: connections.ConnContext}
			go func() { <-ctx.Done(); _ = server.Close() }()
			go connections.Run(ctx, time.Minute)
			if err := server.Serve(listener); !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		},
	}
	command.Flags().String("listen", "127.0.0.1:7447", "the address to listen on")
	command.Flags().String("db", "", "the file that holds the events (default <home>/relay.db)")
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

// relayHandler serves the relay, and the HTTP paths of TURN and rings when
// the relay offers them. Every other path is the relay's: khatru answers
// 404 on a path that it does not know.
func relayHandler(rl *khatru.Relay, turnServer *turn.Server, ringer *rings.Rings) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", rl)
	if turnServer != nil {
		mux.Handle(turn.Path, turnServer)
	}
	if ringer != nil {
		mux.Handle(rings.Path, ringer)
		mux.Handle(rings.TestPath, ringer)
	}
	return mux
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
