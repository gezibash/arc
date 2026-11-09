package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/core/keys"
)

// signerIdentity is who this citizen is on this machine: a secret key in
// the key file, or a remote signer of NIP-46 that the key file names.
type signerIdentity struct {
	key    keys.Key
	signer keys.Signer
	remote bool
}

// loadIdentity reads the key file. It holds a secret key as hex or nsec, an
// ncryptsec of NIP-49 that a passphrase opens, or a bunker URI of NIP-46.
func loadIdentity(ctx context.Context, dir string) (signerIdentity, error) {
	text, err := keyfile.Read(keyPath(dir))
	if err != nil {
		return signerIdentity{}, err
	}
	switch {
	case strings.HasPrefix(text, "bunker://"):
		return remoteIdentity(ctx, dir, text)
	case strings.HasPrefix(text, "ncryptsec1"):
		passphrase, err := askPassphrase("passphrase of the key: ")
		if err != nil {
			return signerIdentity{}, err
		}
		k, err := keys.Decrypt(text, passphrase)
		if err != nil {
			return signerIdentity{}, err
		}
		return signerIdentity{key: k, signer: k}, nil
	}
	k, err := keys.Parse(text)
	if err != nil {
		return signerIdentity{}, err
	}
	return signerIdentity{key: k, signer: k}, nil
}

// remoteIdentity is an identity of a NIP-46 signer. This machine keeps a key
// of its own for the connection, in <home>/bunker-client; it signs nothing
// else. An identity that this machine added knows its public key, so it
// connects at the first signature or decryption. Thus a command that needs
// neither runs with no path to the signer. While it adds the identity, it
// connects at once, to learn the key.
func remoteIdentity(ctx context.Context, dir, uri string) (signerIdentity, error) {
	if err := ctx.Err(); err != nil {
		return signerIdentity{}, err
	}
	text, err := os.ReadFile(publicPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		signer, pk, err := connectSigner(ctx, dir, uri)
		if err != nil {
			return signerIdentity{}, err
		}
		return signerIdentity{key: keys.Key{Public: pk}, signer: keys.Identity{Public: pk, Keyer: signer}, remote: true}, nil
	}
	if err != nil {
		return signerIdentity{}, err
	}
	pk, err := nostr.PubKeyFromHex(strings.TrimSpace(string(text)))
	if err != nil {
		return signerIdentity{}, fmt.Errorf("%s holds no public key: %w", publicPath(dir), err)
	}
	lazy := &lazySigner{public: pk, now: time.Now, silent: filepath.Join(dir, "signer-silent"), connect: func() (nostr.Keyer, error) {
		signer, got, err := connectSigner(ctx, dir, uri)
		if err == nil && got != pk {
			err = fmt.Errorf("the remote signer signs as %s, not as this identity %s", got.Hex(), pk.Hex())
		}
		return signer, err
	}}
	return signerIdentity{key: keys.Key{Public: pk}, signer: keys.Identity{Public: pk, Keyer: lazy}, remote: true}, nil
}

// connectSigner connects to a NIP-46 signer, and asks for its public key.
func connectSigner(ctx context.Context, dir, uri string) (nostr.Keyer, nostr.PubKey, error) {
	// Keep a successful connection for the command lifetime. Stop a failed or
	// abandoned handshake so its subscriptions cannot outlive the attempt.
	connectCtx, stop := context.WithCancel(ctx)
	connected := false
	defer func() {
		if !connected {
			stop()
		}
	}()
	path := filepath.Join(dir, "bunker-client")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := keyfile.Save(path, keys.Generate()); err != nil {
			return nil, nostr.PubKey{}, err
		}
	}
	client, err := keyfile.Load(path)
	if err != nil {
		return nil, nostr.PubKey{}, err
	}
	// The client listens for answers for as long as its context lives, so
	// the context is the command's own. A timer bounds the first contact.
	type result struct {
		signer nostr.Keyer
		pk     nostr.PubKey
		err    error
	}
	done := make(chan result, 1)
	go func() {
		signer, err := keyer.New(connectCtx, nil, uri, &keyer.SignerOptions{BunkerClientSecretKey: client.Secret, BunkerSignTimeout: 15 * time.Second})
		if err != nil {
			done <- result{err: fmt.Errorf("the remote signer did not answer: %w", err)}
			return
		}
		pk, err := signer.GetPublicKey(connectCtx)
		if err != nil {
			err = fmt.Errorf("the remote signer did not give its key: %w", err)
		}
		done <- result{signer, pk, err}
	}()
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	select {
	case r := <-done:
		if err := ctx.Err(); err != nil {
			return nil, nostr.PubKey{}, err
		}
		if r.err != nil {
			return nil, nostr.PubKey{}, r.err
		}
		connected = true
		return timed{r.signer}, r.pk, nil
	case <-ctx.Done():
		return nil, nostr.PubKey{}, ctx.Err()
	case <-timer.C:
		return nil, nostr.PubKey{}, fmt.Errorf("the remote signer did not answer in 15 seconds: %w", context.DeadlineExceeded)
	}
}
