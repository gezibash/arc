package main

import (
	"fmt"
	"path/filepath"

	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/citizen"
	"github.com/spf13/cobra"
)

// session is one open store, with the key and the relays of this citizen.
func open(command *cobra.Command) (*citizen.Session, error) {
	dir, err := home(command)
	if err != nil {
		return nil, err
	}
	return openDir(command, dir)
}

// openDir opens the session of the identity in dir.
func openDir(command *cobra.Command, dir string) (*citizen.Session, error) {
	id, err := loadIdentity(command.Context(), dir)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			id.done()
		}
	}()
	urls, err := readRelays(dir)
	if err != nil {
		return nil, err
	}
	indexers, err := readIndexers(dir)
	if err != nil {
		return nil, err
	}
	root, err := rootDir(command)
	if err != nil {
		return nil, err
	}
	var device keys.Key
	if id.remote {
		if device, err = keyfile.Load(filepath.Join(dir, "bunker-client")); err != nil {
			return nil, err
		}
	}
	keep = true
	sess, err := citizen.Open(citizen.Config{Home: dir, Root: root, Key: id.key, Signer: id.signer, Holder: id.holder, Remote: id.remote, Device: device, URLs: urls, IndexerURLs: indexers, Errors: command.ErrOrStderr(), Close: id.close})
	if err != nil {
		id.done()
	}
	return sess, err
}

func errRemote(what string) error {
	return fmt.Errorf("%s needs the secret key on this machine; a remote signer cannot do it yet", what)
}
