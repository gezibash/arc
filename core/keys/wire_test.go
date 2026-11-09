package keys

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// through is a Remote for k that sends each request through Answer, as a
// program signer does after the JSON lines.
func through(k Key) Remote {
	return Remote{Public: k.Public, Call: func(ctx context.Context, method string, params ...string) (string, error) {
		r := k.Answer(ctx, Request{ID: "1", Method: method, Params: params})
		if r.Error != "" {
			return "", errors.New(r.Error)
		}
		return r.Result, nil
	}}
}

// TestRemoteAnswers checks that each request through a signer gives what
// the key gives itself, and that each signature verifies against the
// digest that the spec defines.
func TestRemoteAnswers(t *testing.T) {
	ctx := context.Background()
	k := Generate()
	r := through(k)
	storage, account := Generate().Public, Generate().Public

	root, err := r.KeyedRoot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := KeyedRoot(k.Secret)
	if !bytes.Equal(root, want) {
		t.Fatal("the keyed root through the signer differs")
	}

	got, err := r.AccountKey(ctx, storage)
	if err != nil {
		t.Fatal(err)
	}
	if direct, _ := AccountKey(k.Secret, storage); got.Secret != direct.Secret {
		t.Fatal("the account key through the signer differs")
	}

	pub, _ := schnorr.ParsePubKey(k.Public[:])
	verify := func(name, sig string, digest [32]byte) {
		t.Helper()
		parsed, err := schnorr.ParseSignature(mustHex(t, sig))
		if err != nil || !parsed.Verify(digest[:], pub) {
			t.Fatalf("%s: the signature does not verify", name)
		}
	}
	sig, err := r.SignJoin(ctx, account, storage)
	if err != nil {
		t.Fatal(err)
	}
	verify("join", sig, JoinDigest(account, storage))

	encoded := []byte(`{"channel":"stable"}`)
	if sig, err = r.SignRelease(ctx, 2, encoded); err != nil {
		t.Fatal(err)
	}
	verify("release", sig, ReleaseDigest(2, encoded))

	record := nostr.ID{7}
	if sig, err = r.SignChange(ctx, "blob", record); err != nil {
		t.Fatal(err)
	}
	verify("change", sig, ChangeDigest("blob", record))

	event := nostr.Event{Kind: 1, Content: "hi", CreatedAt: nostr.Now()}
	if err := r.SignEvent(ctx, &event); err != nil {
		t.Fatal(err)
	}
	if event.PubKey != k.Public || !event.VerifySignature() {
		t.Fatal("the event through the signer does not verify")
	}

	peer := Generate()
	sealed, err := r.Encrypt(ctx, "secret", peer.Public)
	if err != nil {
		t.Fatal(err)
	}
	if text, err := peer.Decrypt(ctx, sealed, k.Public); err != nil || text != "secret" {
		t.Fatalf("NIP-44 through the signer: %q, %v", text, err)
	}
}

// TestRemoteRefusesAnotherKey checks that a signer that signs with another
// key gives an error, not an event of the wrong author.
func TestRemoteRefusesAnotherKey(t *testing.T) {
	k, other := Generate(), Generate()
	r := through(other)
	r.Public = k.Public
	event := nostr.Event{Kind: 1, CreatedAt: nostr.Now()}
	if err := r.SignEvent(context.Background(), &event); err == nil {
		t.Fatal("an event that another key signed was taken")
	}
}

// TestRemoteRefusesAnotherEvent checks that a signer that signs other
// content than it got gives an error.
func TestRemoteRefusesAnotherEvent(t *testing.T) {
	k := Generate()
	r := Remote{Public: k.Public, Call: func(ctx context.Context, method string, params ...string) (string, error) {
		var event nostr.Event
		if err := event.UnmarshalJSON([]byte(params[0])); err != nil {
			return "", err
		}
		event.Content = "something else"
		if err := k.SignEvent(ctx, &event); err != nil {
			return "", err
		}
		return event.String(), nil
	}}
	event := nostr.Event{Kind: 1, Content: "what I wrote", CreatedAt: nostr.Now()}
	if err := r.SignEvent(context.Background(), &event); err == nil || event.Content != "what I wrote" {
		t.Fatalf("an event with other content was taken: %q, %v", event.Content, err)
	}
}

// TestAnswerRefuses checks the answers to a request that the signer cannot
// answer.
func TestAnswerRefuses(t *testing.T) {
	k := Generate()
	for _, req := range []Request{
		{ID: "1", Method: "unknown"},
		{ID: "2", Method: MethodAccountKey},
		{ID: "3", Method: MethodSignRelease, Params: []string{"x", "00"}},
		{ID: "4", Method: MethodSignChange, Params: []string{"blob", "not hex"}},
	} {
		r := k.Answer(context.Background(), req)
		if r.Error == "" || r.ID != req.ID || r.Result != "" {
			t.Errorf("%s: %+v", req.Method, r)
		}
	}
	if !HolderMethod(MethodSignJoin) || HolderMethod("sign_event") {
		t.Fatal("HolderMethod names the wrong methods")
	}
}

func mustHex(t *testing.T, text string) []byte {
	t.Helper()
	raw, err := hex.DecodeString(text)
	if err != nil {
		t.Fatalf("%q is not hex", text)
	}
	return raw
}
