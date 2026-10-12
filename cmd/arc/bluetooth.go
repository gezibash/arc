package main

import (
	"errors"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/spf13/cobra"
)

func bluetoothConfig(command *cobra.Command, dir string) (*citizen.BluetoothConfig, error) {
	peer, _ := command.Flags().GetString("bluetooth-peer")
	address, _ := command.Flags().GetString("bluetooth-address")
	adapter, _ := command.Flags().GetString("bluetooth-adapter")
	listen, _ := command.Flags().GetBool("bluetooth-listen")
	if peer == "" {
		if address != "" || listen || command.Flags().Changed("bluetooth-adapter") {
			return nil, errors.New("--bluetooth-peer is required with Bluetooth options")
		}
		return nil, nil
	}
	if listen && command.Name() != "serve" && !(command.Name() == "watch" && command.Parent() != nil && command.Parent().Name() == "message") {
		return nil, errors.New("--bluetooth-listen is supported by arc serve and arc message watch")
	}
	for _, name := range []string{"dir", "sync-dir", "repair", "turn", "import"} {
		if command.Flags().Changed(name) {
			return nil, errors.New("Bluetooth mode cannot combine with --" + name)
		}
	}
	pk, err := nostr.PubKeyFromHex(peer)
	if err != nil || pk == (nostr.PubKey{}) {
		return nil, errors.New("--bluetooth-peer must be the other citizen's 64-character public key")
	}
	if listen == (address != "") {
		return nil, errors.New("choose --bluetooth-address or --bluetooth-listen")
	}
	// Check before loadIdentity can connect to a remote signer.
	text, err := keyfile.Read(keyPath(dir))
	if err != nil {
		return nil, err
	}
	if strings.HasPrefix(text, "bunker://") {
		return nil, errors.New("bluetooth mode requires a local signing key")
	}
	return &citizen.BluetoothConfig{Peer: pk, Address: address, Adapter: adapter, Listen: listen}, nil
}
