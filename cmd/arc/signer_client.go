package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/citizen"
)

// timed bounds each request to a remote signer, so a lost answer fails the
// command instead of stopping it.
// signerRetry is the time after a failed connection to the signer in which
// each use fails at once with the same reason.
const signerRetry = time.Minute

// lazySigner connects to a remote signer at its first use. After a failed
// connection, each use in the next signerRetry fails at once, so a command
// waits for the signer only once. When the signer did not answer, the file
// silent keeps the time, so the next commands of this machine fail at once
// too. A later use connects again, so arc serve finds the signer when it
// comes back.
type lazySigner struct {
	public  nostr.PubKey
	connect func() (nostr.Keyer, error)
	now     func() time.Time
	silent  string
	mu      sync.Mutex
	keyer   nostr.Keyer
	err     error
	failed  time.Time
}

func (l *lazySigner) get() (nostr.Keyer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.keyer != nil {
		return l.keyer, nil
	}
	if l.err != nil && l.now().Sub(l.failed) < signerRetry {
		return nil, l.err
	}
	if at, ok := l.silentAt(); ok && l.now().Sub(at) < signerRetry {
		l.err, l.failed = &citizen.SignerError{Citizen: l.public, Err: context.DeadlineExceeded, Retry: at.Add(signerRetry)}, at
		return nil, l.err
	}
	k, err := l.connect()
	if err != nil {
		l.failed = l.now()
		signerErr := &citizen.SignerError{Citizen: l.public, Err: err}
		if errors.Is(err, context.DeadlineExceeded) {
			signerErr.Retry = l.failed.Add(signerRetry)
			l.keepSilent(l.failed)
		}
		l.err = signerErr
		return nil, l.err
	}
	if l.silent != "" {
		_ = os.Remove(l.silent)
	}
	l.keyer, l.err = k, nil
	return k, nil
}

// silentAt reads the time at which the signer last did not answer. A
// missing or broken file reads as no time.
func (l *lazySigner) silentAt() (time.Time, bool) {
	if l.silent == "" {
		return time.Time{}, false
	}
	text, err := os.ReadFile(l.silent)
	if err != nil {
		return time.Time{}, false
	}
	at, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(text)))
	return at, err == nil
}

// keepSilent keeps the time at which the signer did not answer. A failed
// write only costs the next command one more wait.
func (l *lazySigner) keepSilent(at time.Time) {
	if l.silent != "" {
		_ = os.WriteFile(l.silent, []byte(at.Format(time.RFC3339Nano)+"\n"), 0o600)
	}
}

func (l *lazySigner) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	return l.public, ctx.Err()
}

func (l *lazySigner) SignEvent(ctx context.Context, event *nostr.Event) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.SignEvent(ctx, event)
}

func (l *lazySigner) Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	k, err := l.get()
	if err != nil {
		return "", err
	}
	return k.Encrypt(ctx, plaintext, to)
}

func (l *lazySigner) Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	k, err := l.get()
	if err != nil {
		return "", err
	}
	return k.Decrypt(ctx, ciphertext, from)
}

func (l *lazySigner) Nip04Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	k, err := l.get()
	if err != nil {
		return "", err
	}
	return k.Nip04Encrypt(ctx, plaintext, to)
}

func (l *lazySigner) Nip04Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	k, err := l.get()
	if err != nil {
		return "", err
	}
	return k.Nip04Decrypt(ctx, ciphertext, from)
}

type timed struct{ nostr.Keyer }

const signerTimeout = 20 * time.Second

// signerError preserves cancellation identity when the remote library returns
// an untyped network error. A signer refusal under an active context is unchanged.
func signerError(ctx context.Context, err error) error {
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (t timed) SignEvent(ctx context.Context, event *nostr.Event) error {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	return signerError(ctx, t.Keyer.SignEvent(ctx, event))
}

func (t timed) Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err := t.Keyer.Encrypt(ctx, plaintext, to)
	return text, signerError(ctx, err)
}

func (t timed) Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err := t.Keyer.Decrypt(ctx, ciphertext, from)
	return text, signerError(ctx, err)
}

func (t timed) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nostr.PubKey{}, err
	}
	pk, err := t.Keyer.GetPublicKey(ctx)
	return pk, signerError(ctx, err)
}

func (t timed) Nip04Encrypt(ctx context.Context, plaintext string, to nostr.PubKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err := t.Keyer.Nip04Encrypt(ctx, plaintext, to)
	return text, signerError(ctx, err)
}

func (t timed) Nip04Decrypt(ctx context.Context, ciphertext string, from nostr.PubKey) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, signerTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return "", err
	}
	text, err := t.Keyer.Nip04Decrypt(ctx, ciphertext, from)
	return text, signerError(ctx, err)
}
