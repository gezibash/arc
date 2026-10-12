package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/devices"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/spf13/cobra"
)

// deviceKeyPath is the device key of this machine. A new identity that
// signs through a bunker uses it as its client key. An identity added
// before the device key keeps its own <home>/bunker-client.
func deviceKeyPath(root string) string { return filepath.Join(root, "device") }

// deviceKey reads the device key of this machine, and makes it first if
// there is none. The key is random and never leaves the machine.
func deviceKey(root string) (keys.Key, error) {
	path := deviceKeyPath(root)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return keys.Key{}, err
		}
		if err := keyfile.Save(path, keys.Generate()); err != nil {
			return keys.Key{}, err
		}
	}
	return keyfile.Load(path)
}

func deviceCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "device",
		Short: "Show the device key of this machine",
		Long: "arc prints the device key of this machine, and makes it first if there\n" +
			"is none. The key is random and never leaves the machine. Give it to\n" +
			"the signer: arc keys devices add <key> --name <name> runs there.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			root, err := rootDir(command)
			if err != nil {
				return err
			}
			k, err := deviceKey(root)
			if err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), k.Public.Hex())
			return nil
		},
	}
}

func accountCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "account <key of the storage citizen>",
		Short: "Show the account key of this citizen for a storage citizen",
		Long: "arc derives the account key of this citizen for one storage citizen\n" +
			"from the citizen key, and prints its public half. Give it to the\n" +
			"operator of the storage citizen for STORAGE_OWNERS. Two storage\n" +
			"citizens get two account keys, and neither sees the citizen key.\n" +
			"Run this on the signer.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			storage, ok := keys.ParsePublic(args[0])
			if !ok {
				return fmt.Errorf("%q is not the key of a storage citizen", args[0])
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			account, err := sess.AccountKey(storage)
			if err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), account.Public.Hex())
			return nil
		},
	}
}

func devicesCommand() *cobra.Command {
	command := &cobra.Command{
		Use:   "devices",
		Short: "List, add and revoke the devices that sign through this signer",
		Long: "The device list names each device key that may sign and decrypt as\n" +
			"this citizen through arc keys bunker. The citizen key signs the list\n" +
			"and seals it to itself; only the signer changes it. When a list\n" +
			"exists, the bunker admits only the devices on it.",
		Args: cobra.NoArgs,
		RunE: listDevices,
	}
	list := &cobra.Command{Use: "list", Short: "List the devices", Args: cobra.NoArgs, RunE: listDevices}

	add := &cobra.Command{
		Use:   "add <device key>",
		Short: "Add a device key to the list",
		Long:  "The device prints its key with arc keys device. Run this on the signer.",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			key, err := nostr.PubKeyFromHex(args[0])
			if err != nil {
				return fmt.Errorf("%q is not a device key of 64 hex characters: %w", args[0], err)
			}
			name, _ := command.Flags().GetString("name")
			return changeDevices(command, func(l devices.List) (devices.List, string, error) {
				l, err := l.Add(devices.Device{Key: key, Name: name, Added: nostr.Now()})
				return l, fmt.Sprintf("added %s  %s", name, key.Hex()), err
			})
		},
	}
	add.Flags().String("name", "", "a name for the device, such as phone")
	_ = add.MarkFlagRequired("name")

	revoke := &cobra.Command{
		Use:   "revoke <name or device key>",
		Short: "Remove a device from the list",
		Long: "The signer refuses each later request of the device. A device that\n" +
			"was revoked can be added again with a new device key.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			return changeDevices(command, func(l devices.List) (devices.List, string, error) {
				d, ok := l.Find(args[0])
				if !ok {
					return l, "", fmt.Errorf("no device %q on the list: see arc keys devices", args[0])
				}
				return l.Remove(d.Key), fmt.Sprintf("revoked %s  %s", d.Name, d.Key.Hex()), nil
			})
		},
	}

	command.AddCommand(list, add, revoke)
	return command
}

func listDevices(command *cobra.Command, _ []string) error {
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	l, ok, err := sess.DeviceList(command.Context())
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(command.OutOrStdout(), "no device list: the bunker admits each device that gives its secret")
		return nil
	}
	if len(l.Devices) == 0 {
		fmt.Fprintln(command.OutOrStdout(), "the device list is empty: the bunker admits no device")
	}
	for _, d := range l.Devices {
		fmt.Fprintf(command.OutOrStdout(), "%s  %s  added %s\n", d.Name, d.Key.Hex(), d.Added.Time().UTC().Format(time.DateOnly))
	}
	return nil
}

// changeDevices reads the newest list, changes it, and publishes it.
func changeDevices(command *cobra.Command, change func(devices.List) (devices.List, string, error)) error {
	sess, err := open(command)
	if err != nil {
		return err
	}
	defer sess.Close()
	if err := sess.RequireSigner(); err != nil {
		return err
	}
	l, _, err := sess.DeviceList(command.Context())
	if err != nil {
		return err
	}
	l, said, err := change(l)
	if err != nil {
		return err
	}
	l, err = sess.PublishDeviceList(command.Context(), l)
	if err != nil {
		return err
	}
	fmt.Fprintln(command.OutOrStdout(), said)
	root, err := sess.PublishDataRoots(command.Context(), l)
	if err != nil {
		return fmt.Errorf("the device list changed, but the data roots did not: %w; run arc keys roots on the signer to try again", err)
	}
	fmt.Fprintf(command.OutOrStdout(), "data roots: generation %d\n", root.Gen)
	installs, err := installsOf(command)
	if err != nil {
		return err
	}
	targets, err := sess.PushGrants(command.Context(), installs, l)
	for _, install := range targets {
		fmt.Fprintf(command.OutOrStdout(), "grants: sent to the storage citizen %s\n", install.As)
	}
	if err != nil {
		return fmt.Errorf("the device list changed, but %w; run arc sync on the signer to try again", err)
	}
	return nil
}

func rootsCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "roots",
		Short: "List the generations of data roots that this machine holds",
		Long: "A data root seals the self data of the citizen. The signer wraps the\n" +
			"newest root to each device on the device list and to the citizen key,\n" +
			"and makes a new generation when it revokes a device. Generation 0 is\n" +
			"the keyed root, which follows from the citizen key. On the signer, this\n" +
			"command first makes the data roots follow the device list.",
		Args: cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			ctx := command.Context()
			var holders []string
			if !sess.Remote {
				l, ok, err := sess.DeviceList(ctx)
				if err != nil {
					return err
				}
				if ok {
					if _, err := sess.PublishDataRoots(ctx, l); err != nil {
						return err
					}
				}
				if r, ok, err := sess.NewestRoots(ctx); err != nil {
					return err
				} else if ok {
					for key := range r.Devices {
						name := "a device not on the list"
						if d, found := l.Find(key.Hex()); found {
							name = d.Name
						}
						holders = append(holders, fmt.Sprintf("  wrapped to %s  %s", name, key.Hex()))
					}
					slices.Sort(holders)
				}
			}
			held, err := sess.DataRoots(ctx)
			if len(held) == 0 {
				return err
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "arc: the newest data roots did not come: %v\n", err)
			}
			gens := slices.Sorted(maps.Keys(held))
			slices.Reverse(gens)
			for i, gen := range gens {
				if i == 0 {
					fmt.Fprintf(command.OutOrStdout(), "generation %d (newest)\n", gen)
					for _, h := range holders {
						fmt.Fprintln(command.OutOrStdout(), h)
					}
				} else {
					fmt.Fprintf(command.OutOrStdout(), "generation %d\n", gen)
				}
			}
			return nil
		},
	}
}

func sealCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "seal <app>",
		Short: "Seal standard input for an app with the newest data root",
		Long: "arc encrypts standard input with the content key of the app under the\n" +
			"newest data root, and prints the generation, a colon and the NIP-44\n" +
			"payload. arc keys unseal opens it on each machine that holds the root.",
		Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			text, err := io.ReadAll(command.InOrStdin())
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			root, err := sess.DataRoot(command.Context())
			if err != nil {
				return err
			}
			sealed, err := devices.SealData(root, args[0], string(text))
			if err != nil {
				return err
			}
			fmt.Fprintln(command.OutOrStdout(), sealed)
			return nil
		},
	}
}

func unsealCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unseal <app>",
		Short: "Open data that arc keys seal sealed for an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			sealed, err := io.ReadAll(command.InOrStdin())
			if err != nil {
				return err
			}
			text := strings.TrimSpace(string(sealed))
			gen, err := devices.DataGeneration(text)
			if err != nil {
				return err
			}
			sess, err := open(command)
			if err != nil {
				return err
			}
			defer sess.Close()
			root, err := sess.DataRootAt(command.Context(), gen)
			if err != nil {
				return err
			}
			plain, err := devices.OpenData(root, args[0], text)
			if err != nil {
				return err
			}
			fmt.Fprint(command.OutOrStdout(), plain)
			return nil
		},
	}
}

// guard decides which devices a bunker admits. With no device list, it
// admits a device that gave the bunker secret once, as before device lists.
// With a list, it admits only the device keys on the list.
type guard struct {
	mu         sync.Mutex
	secret     string
	authorized map[nostr.PubKey]bool
	list       *devices.List
}

func (g *guard) authorize(from nostr.PubKey, given string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil {
		return g.list.Has(from)
	}
	if given == g.secret {
		g.authorized[from] = true
	}
	return g.authorized[from]
}

// refusal says why the bunker refuses a device, or nothing.
func (g *guard) refusal(from nostr.PubKey) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil && !g.list.Has(from) {
		return errors.New("the signer refuses this device: its key is not on the device list")
	}
	return nil
}

// load takes a list when it is newer than the list that the guard holds.
func (g *guard) load(l devices.List) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.list != nil && l.At <= g.list.At {
		return false
	}
	g.list = &l
	return true
}

func (g *guard) listed() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.list != nil
}

// watchDevices loads each new device list that reaches the bunker's relay,
// until ctx ends. The relay serves the sealed list only after the bunker
// authenticates as the citizen.
func watchDevices(ctx context.Context, r relay.Relay, k keys.Key, g *guard, log *slog.Logger) {
	for {
		events, err := r.Watch(ctx, devices.Filter(k.Public))
		if err == nil {
			for event := range events {
				l, err := devices.Open(ctx, k, k.Public, event)
				if err != nil {
					log.Warn("a device list does not open", "error", err)
					continue
				}
				if g.load(l) {
					log.Info("loaded the device list", "devices", len(l.Devices))
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		log.Warn("the relay ended the watch of the device list; watching again", "relay", r.URL, "error", err)
		select {
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
			return
		}
	}
}

// startDevices loads the newest device list that the store and the relays
// hold, before the bunker answers anyone.
func startDevices(ctx context.Context, sess *citizen.Session, r relay.Relay, g *guard) error {
	filter := devices.Filter(sess.Key.Public)
	sess.Node.Pull(ctx, filter, sess.Relays)
	held, err := sess.Node.Store.Query(filter)
	if err != nil {
		return err
	}
	if batch, err := r.Fetch(ctx, filter); err == nil {
		held = append(held, batch.Events...)
	} else {
		fmt.Fprintf(os.Stderr, "the device list did not come from %s: %v\n", r.URL, err)
	}
	if l, ok, err := citizen.NewestDeviceList(ctx, sess.Signer, sess.Key.Public, held); err != nil {
		return err
	} else if ok {
		g.load(l)
	}
	return nil
}
