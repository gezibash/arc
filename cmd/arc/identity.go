package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"github.com/gezibash/arc/adapters/keyfile"
	"github.com/gezibash/arc/adapters/signer/program"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/runtime/citizen"
)

// signerIdentity is who this citizen is on this machine: a program signer
// that holds the key here, or a remote signer of NIP-46. arc never holds
// the secret key.
type signerIdentity struct {
	// key is the public key of the citizen.
	key    keys.Key
	signer keys.Signer
	// holder answers the requests that need the secret. A remote signer
	// gives none.
	holder citizen.Holder
	remote bool
	// close ends the program signer, if one runs.
	close func() error
}

// done ends the program signer of the identity, if one runs.
func (id signerIdentity) done() {
	if id.close != nil {
		_ = id.close()
	}
}

// signerProgram is the program signer that arc gives a new identity.
const signerProgram = "arc-signer"

// loadIdentity reads the key file. It names a program signer, as
// program:<name>, or a bunker URI of NIP-46. A key file of an older arc
// holds the secret itself: arc moves it to arc-signer first.
func loadIdentity(ctx context.Context, dir string) (signerIdentity, error) {
	text, err := keyfile.Read(keyPath(dir))
	if err != nil {
		return signerIdentity{}, err
	}
	switch {
	case strings.HasPrefix(text, "bunker://"):
		return remoteIdentity(ctx, dir, text)
	case strings.HasPrefix(text, program.Prefix):
		return programIdentity(dir, strings.TrimPrefix(text, program.Prefix))
	}
	if err := moveToSigner(ctx, dir); err != nil {
		return signerIdentity{}, err
	}
	return programIdentity(dir, signerProgram)
}

// programIdentity is an identity whose key a program signer on this
// machine holds. The program starts at the first request.
func programIdentity(dir, name string) (signerIdentity, error) {
	text, err := os.ReadFile(publicPath(dir))
	if err != nil {
		return signerIdentity{}, err
	}
	pk, err := nostr.PubKeyFromHex(strings.TrimSpace(string(text)))
	if err != nil {
		return signerIdentity{}, fmt.Errorf("%s holds no public key: %w", publicPath(dir), err)
	}
	path, err := program.Find(name)
	if err != nil {
		return signerIdentity{}, err
	}
	client := &program.Client{Program: path, Dir: dir}
	remote := keys.Remote{Public: pk, Call: client.Call}
	return signerIdentity{key: keys.Key{Public: pk}, signer: remote, holder: remote, close: client.Close}, nil
}

// moveToSigner moves the secret of the key file of an older arc to
// arc-signer, and names arc-signer in the key file. The public key stays.
func moveToSigner(ctx context.Context, dir string) error {
	path, err := program.Find(signerProgram)
	if err != nil {
		return err
	}
	args := []string{"import", "--dir", dir, "--from", keyPath(dir)}
	if public, err := os.ReadFile(publicPath(dir)); err == nil {
		args = append(args, "--public", strings.TrimSpace(string(public)))
	}
	command := exec.CommandContext(ctx, path, args...)
	command.Stdin, command.Stderr = os.Stdin, os.Stderr
	out, err := command.Output()
	if err != nil {
		return fmt.Errorf("arc-signer did not take the key of %s: %w", filepath.Base(dir), err)
	}
	public := strings.TrimSpace(string(out))
	if _, err := nostr.PubKeyFromHex(public); err != nil {
		return fmt.Errorf("arc-signer gave no public key: %q", public)
	}
	if err := os.WriteFile(publicPath(dir), []byte(public+"\n"), 0o600); err != nil {
		return err
	}
	return keyfile.Write(keyPath(dir), program.Prefix+signerProgram)
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
