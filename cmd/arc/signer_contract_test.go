package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip46"
	"github.com/gezibash/arc/adapters/transport/relay"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/internal/testrelay"
)

// remoteForTest exercises the actual NIP-46 adapter through a loopback relay.
func remoteForTest(t *testing.T) (keys.Signer, keys.Key) {
	t.Helper()
	owner := keys.Generate()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	r := relay.Relay{URL: testrelay.Start(t)}
	requests, err := r.Watch(ctx, nostr.Filter{Kinds: []nostr.Kind{nostr.KindNostrConnect}, Tags: nostr.TagMap{"p": {owner.Public.Hex()}}})
	if err != nil {
		t.Fatal(err)
	}
	bunker := nip46.NewStaticKeySigner(owner.Secret)
	bunker.AuthorizeRequest = func(bool, nostr.PubKey, string) bool { return true }
	done := make(chan struct{})
	go func() {
		defer close(done)
		for request := range requests {
			response, ok := answer(ctx, &bunker, owner.Secret, request, policy{decrypt: "all"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if ok {
				_ = r.Send(ctx, response)
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	identity, err := remoteIdentity(ctx, t.TempDir(), fmt.Sprintf("bunker://%s?relay=%s", owner.Public.Hex(), r.URL))
	if err != nil {
		t.Fatal(err)
	}
	return identity.signer, owner
}

func keyerOperations(k nostr.Keyer) map[string]func(context.Context) error {
	peer := keys.Generate().Public
	return map[string]func(context.Context) error{
		"public_key": func(ctx context.Context) error { _, err := k.GetPublicKey(ctx); return err },
		"sign": func(ctx context.Context) error {
			event := nostr.Event{Kind: 1, Content: "contract", Tags: nostr.Tags{}}
			return k.SignEvent(ctx, &event)
		},
		"encrypt":       func(ctx context.Context) error { _, err := k.Encrypt(ctx, "hello", peer); return err },
		"decrypt":       func(ctx context.Context) error { _, err := k.Decrypt(ctx, "not ciphertext", peer); return err },
		"nip04_encrypt": func(ctx context.Context) error { _, err := k.Nip04Encrypt(ctx, "hello", peer); return err },
		"nip04_decrypt": func(ctx context.Context) error { _, err := k.Nip04Decrypt(ctx, "not ciphertext", peer); return err },
	}
}

func TestSignerContextContract(t *testing.T) {
	factories := map[string]func(*testing.T) (keys.Signer, keys.Key){
		"local":  func(*testing.T) (keys.Signer, keys.Key) { k := keys.Generate(); return k, k },
		"remote": remoteForTest,
	}
	for name, factory := range factories {
		t.Run(name, func(t *testing.T) {
			signer, owner := factory(t)
			ctx := context.Background()
			pk, err := signer.GetPublicKey(ctx)
			if err != nil || pk != owner.Public || signer.PublicKey() != owner.Public {
				t.Fatalf("public identity mismatch: %v", err)
			}
			event := nostr.Event{Kind: 1, CreatedAt: nostr.Now(), Content: "adapter contract", Tags: nostr.Tags{}}
			if err := signer.SignEvent(ctx, &event); err != nil || event.PubKey != owner.Public || !event.CheckID() || !event.VerifySignature() {
				t.Fatalf("invalid signed event: %v", err)
			}
			peer := keys.Generate()
			for _, legacy := range []bool{false, true} {
				encrypt, decrypt := signer.Encrypt, signer.Decrypt
				peerEncrypt, peerDecrypt := peer.Encrypt, peer.Decrypt
				if legacy {
					encrypt, decrypt = signer.Nip04Encrypt, signer.Nip04Decrypt
					peerEncrypt, peerDecrypt = peer.Nip04Encrypt, peer.Nip04Decrypt
				}
				const message = "one signed identity, two implementations"
				ciphertext, err := encrypt(ctx, message, peer.Public)
				if err != nil {
					t.Fatal(err)
				}
				plaintext, err := peerDecrypt(ctx, ciphertext, owner.Public)
				if err != nil || plaintext != message {
					t.Fatalf("encrypt interoperability: %q, %v", plaintext, err)
				}
				ciphertext, err = peerEncrypt(ctx, message, owner.Public)
				if err != nil {
					t.Fatal(err)
				}
				plaintext, err = decrypt(ctx, ciphertext, peer.Public)
				if err != nil || plaintext != message {
					t.Fatalf("decrypt interoperability: %q, %v", plaintext, err)
				}
			}
			for name, op := range keyerOperations(signer) {
				t.Run(name, func(t *testing.T) {
					for _, expired := range []bool{false, true} {
						ctx, cancel := context.WithCancel(context.Background())
						if expired {
							cancel()
							ctx, cancel = context.WithDeadline(context.Background(), time.Unix(1, 0))
						}
						cancel()
						if err := op(ctx); !errors.Is(err, ctx.Err()) {
							t.Errorf("error = %v, want %v", err, ctx.Err())
						}
					}
				})
			}
		})
	}
}

type observedKeyer struct {
	nostr.Keyer
	run func(context.Context) error
}

func (k observedKeyer) GetPublicKey(ctx context.Context) (nostr.PubKey, error) {
	return nostr.PubKey{}, k.run(ctx)
}
func (k observedKeyer) SignEvent(ctx context.Context, _ *nostr.Event) error { return k.run(ctx) }
func (k observedKeyer) Encrypt(ctx context.Context, _ string, _ nostr.PubKey) (string, error) {
	return "", k.run(ctx)
}
func (k observedKeyer) Decrypt(ctx context.Context, _ string, _ nostr.PubKey) (string, error) {
	return "", k.run(ctx)
}
func (k observedKeyer) Nip04Encrypt(ctx context.Context, _ string, _ nostr.PubKey) (string, error) {
	return "", k.run(ctx)
}
func (k observedKeyer) Nip04Decrypt(ctx context.Context, _ string, _ nostr.PubKey) (string, error) {
	return "", k.run(ctx)
}

func TestTimedSignerContract(t *testing.T) {
	probe := observedKeyer{}
	for _, method := range []string{"public_key", "sign", "encrypt", "decrypt", "nip04_encrypt", "nip04_decrypt"} {
		t.Run(method, func(t *testing.T) {
			refusal := errors.New("signer policy refused the request")
			probe.run = func(ctx context.Context) error {
				deadline, ok := ctx.Deadline()
				if !ok || time.Until(deadline) > 20*time.Second {
					t.Error("the signer request has no bounded deadline")
				}
				return refusal
			}
			if err := keyerOperations(timed{probe})[method](context.Background()); !errors.Is(err, refusal) {
				t.Fatalf("refusal = %v", err)
			}
			for _, expire := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				if expire {
					cancel()
					ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
				}
				defer cancel()
				probe.run = func(got context.Context) error {
					if want, ok := ctx.Deadline(); ok {
						if deadline, ok := got.Deadline(); !ok || !deadline.Equal(want) {
							t.Error("the earlier caller deadline was changed")
						}
					}
					if !expire {
						cancel()
					}
					<-got.Done()
					return errors.New("upstream lost the cancellation cause")
				}
				if err := keyerOperations(timed{probe})[method](ctx); !errors.Is(err, ctx.Err()) {
					t.Errorf("error = %v, want %v", err, ctx.Err())
				}
			}
		})
	}
}

func TestRemoteIdentityCancellation(t *testing.T) {
	t.Run("before_connect", func(t *testing.T) {
		home := t.TempDir()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := remoteIdentity(ctx, home, "bunker://"+keys.Generate().Public.Hex()+"?relay=ws://127.0.0.1:1")
		if !errors.Is(err, context.Canceled) {
			t.Errorf("connect = %v", err)
		}
		if _, err := os.Stat(filepath.Join(home, "bunker-client")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("canceled connect created a client identity: %v", err)
		}
	})
	t.Run("no_signer_reply", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		started := time.Now()
		_, err := remoteIdentity(ctx, t.TempDir(), "bunker://"+keys.Generate().Public.Hex()+"?relay="+testrelay.Start(t))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("connect = %v", err)
		}
		if time.Since(started) > time.Second {
			t.Error("connect outlived the caller deadline")
		}
	})
}
