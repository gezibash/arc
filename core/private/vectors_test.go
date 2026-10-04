package private_test

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip59"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/private"
)

// The vectors in testdata are documented in testdata/README.md.
// route_tags.json and ../mail/testdata/message_rumor.json come from an
// independent computation, not from this package. Only wraps.json is
// recorded by this test, because a seal and a wrap use random keys:
//
//	go test ./core/private -run '^TestWrapVectors$' -update
var update = flag.Bool("update", false, "record testdata/wraps.json again")

type routeVector struct {
	Description string   `json:"description"`
	Recipient   string   `json:"recipient"`
	Time        string   `json:"time"`
	Unix        int64    `json:"unix"`
	Today       string   `json:"today"`
	RouteTag    string   `json:"route_tag"`
	Days        []string `json:"days"`
	RouteTags   []string `json:"route_tags"`
}

// rumorVector is an unsigned event, with its id.
type rumorVector struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      nostr.Tags `json:"tags"`
	Content   string     `json:"content"`
}

type keyVector struct {
	Secret string `json:"secret"`
	Public string `json:"public"`
}

type openVector struct {
	Description     string      `json:"description"`
	RecipientSecret string      `json:"recipient_secret"`
	Form            string      `json:"form"`
	RouteDay        string      `json:"route_day,omitempty"`
	Expires         int64       `json:"expires"`
	Wrap            nostr.Event `json:"wrap"`
	Seal            nostr.Event `json:"seal"`
	SealPlaintext   string      `json:"seal_plaintext"`
	Author          string      `json:"author"`
	Rumor           rumorVector `json:"rumor"`
}

type refuseVector struct {
	Description     string       `json:"description"`
	RecipientSecret string       `json:"recipient_secret"`
	Wrap            *nostr.Event `json:"wrap,omitempty"`
	Seal            *nostr.Event `json:"seal,omitempty"`
	Error           string       `json:"error"`
}

type wrapVectors struct {
	Description string               `json:"description"`
	Keys        map[string]keyVector `json:"keys"`
	Open        []openVector         `json:"open"`
	Refuse      []refuseVector       `json:"refuse"`
}

// errorClasses maps the class of a refuse vector to the text of the error
// that core/private returns. The package has no error values to compare.
var errorClasses = map[string]string{
	"wrap-does-not-open":    "the wrap does not open with this key",
	"seal-does-not-open":    "the seal does not open with this key",
	"rumor-author-mismatch": "the rumor names another author than the seal",
}

// readVectors reads a vector file, and fails if the file is missing or holds
// no vectors.
func readVectors(t *testing.T, path string, into any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the vectors: %v", err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func parseKey(t *testing.T, secret string) keys.Key {
	t.Helper()
	k, err := keys.Parse(secret)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func parsePub(t *testing.T, text string) nostr.PubKey {
	t.Helper()
	pub, err := nostr.PubKeyFromHex(text)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func checkRumor(t *testing.T, got nostr.Event, want rumorVector) {
	t.Helper()
	if got.ID.Hex() != want.ID || got.PubKey.Hex() != want.PubKey || int64(got.CreatedAt) != want.CreatedAt ||
		int(got.Kind) != want.Kind || got.Content != want.Content || !tagsEqual(got.Tags, want.Tags) {
		t.Errorf("rumor %s, want %+v", got, want)
	}
}

func tagsEqual(a, b nostr.Tags) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if strings.Join(a[i], "\x00") != strings.Join(b[i], "\x00") || len(a[i]) != len(b[i]) {
			return false
		}
	}
	return true
}

func TestRouteTagVectors(t *testing.T) {
	var file struct {
		Vectors []routeVector `json:"vectors"`
	}
	readVectors(t, "testdata/route_tags.json", &file)
	if len(file.Vectors) == 0 {
		t.Fatal("testdata/route_tags.json holds no vectors")
	}
	for _, v := range file.Vectors {
		t.Run(v.Description, func(t *testing.T) {
			at, err := time.Parse(time.RFC3339, v.Time)
			if err != nil {
				t.Fatal(err)
			}
			if at.Unix() != v.Unix {
				t.Fatalf("the time %s is %d, the vector says %d", v.Time, at.Unix(), v.Unix)
			}
			recipient := parsePub(t, v.Recipient)
			if got := private.RouteTag(recipient, at); got != v.RouteTag {
				t.Errorf("RouteTag = %s, want %s", got, v.RouteTag)
			}
			got := private.RouteTags(recipient, at)
			if strings.Join(got, ",") != strings.Join(v.RouteTags, ",") {
				t.Errorf("RouteTags = %v, want %v", got, v.RouteTags)
			}
		})
	}
}

func TestWrapVectors(t *testing.T) {
	if *update {
		recordWraps(t)
	}
	var file wrapVectors
	readVectors(t, "testdata/wraps.json", &file)
	if len(file.Open) == 0 || len(file.Refuse) == 0 {
		t.Fatal("testdata/wraps.json holds no open or no refuse vectors")
	}
	ctx := context.Background()

	for _, v := range file.Open {
		t.Run(v.Description, func(t *testing.T) {
			me := parseKey(t, v.RecipientSecret)
			for name, event := range map[string]nostr.Event{"wrap": v.Wrap, "seal": v.Seal} {
				if !event.CheckID() || !event.VerifySignature() {
					t.Errorf("the recorded %s is not signed", name)
				}
			}

			opened, err := private.Unwrap(ctx, me, v.Wrap)
			if err != nil {
				t.Fatal(err)
			}
			if opened.Seal.String() != v.Seal.String() {
				t.Errorf("seal %s, want %s", opened.Seal, v.Seal)
			}
			if opened.Author().Hex() != v.Author {
				t.Errorf("author %s, want %s", opened.Author().Hex(), v.Author)
			}
			checkRumor(t, opened.Rumor, v.Rumor)

			rumor, err := private.OpenSeal(ctx, me, v.Seal)
			if err != nil {
				t.Fatal(err)
			}
			checkRumor(t, rumor, v.Rumor)

			plain, err := me.Decrypt(ctx, v.Seal.Content, v.Seal.PubKey)
			if err != nil || plain != v.SealPlaintext {
				t.Errorf("the seal holds %q, want %q (%v)", plain, v.SealPlaintext, err)
			}

			checkWrapTags(t, v, me.Public)

			// The library's own NIP-59 code opens the recorded wrap too.
			other, err := nip59.GiftUnwrap(v.Wrap, func(from nostr.PubKey, ciphertext string) (string, error) {
				return me.Decrypt(ctx, ciphertext, from)
			})
			if err != nil || other.ID.Hex() != v.Rumor.ID {
				t.Errorf("NIP-59 opened %s (%v)", other, err)
			}
		})
	}

	for _, v := range file.Refuse {
		t.Run(v.Description, func(t *testing.T) {
			want, ok := errorClasses[v.Error]
			if !ok {
				t.Fatalf("unknown error class %q", v.Error)
			}
			me := parseKey(t, v.RecipientSecret)
			var err error
			switch {
			case v.Wrap != nil:
				_, err = private.Unwrap(ctx, me, *v.Wrap)
			case v.Seal != nil:
				_, err = private.OpenSeal(ctx, me, *v.Seal)
			default:
				t.Fatal("the vector holds no wrap and no seal")
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("error %v, want class %s", err, v.Error)
			}
		})
	}
}

func checkWrapTags(t *testing.T, v openVector, recipient nostr.PubKey) {
	t.Helper()
	if exp := v.Wrap.Tags.Find("expiration"); exp == nil || exp[1] != strconv.FormatInt(v.Expires, 10) {
		t.Errorf("expiration tag %v, want %d", exp, v.Expires)
	}
	switch v.Form {
	case "relay":
		if p := v.Wrap.Tags.Find("p"); p == nil || p[1] != recipient.Hex() {
			t.Errorf("p tag %v, want %s", p, recipient.Hex())
		}
	case "courier":
		day, err := time.Parse(time.DateOnly, v.RouteDay)
		if err != nil {
			t.Fatal(err)
		}
		if w := v.Wrap.Tags.Find("w"); w == nil || w[1] != private.RouteTag(recipient, day) {
			t.Errorf("w tag %v, want the route tag of %s", w, v.RouteDay)
		}
		if v.Wrap.Tags.Find("p") != nil {
			t.Error("the courier form carries a p tag")
		}
	default:
		t.Errorf("unknown form %q", v.Form)
	}
}

// The rumor inside the recorded wraps is the message rumor of core/mail,
// which an independent computation gave.
func TestWrapVectorsHoldTheMessageRumor(t *testing.T) {
	var wraps wrapVectors
	readVectors(t, "testdata/wraps.json", &wraps)
	var messages struct {
		Vectors []struct {
			Rumor rumorVector `json:"rumor"`
		} `json:"vectors"`
	}
	readVectors(t, "../mail/testdata/message_rumor.json", &messages)
	if len(wraps.Open) == 0 || len(messages.Vectors) == 0 {
		t.Fatal("no vectors")
	}
	want := messages.Vectors[0].Rumor
	for _, v := range wraps.Open {
		if v.Rumor.ID != want.ID {
			t.Errorf("%s: rumor %s, want the message rumor %s", v.Description, v.Rumor.ID, want.ID)
		}
	}
}

// recordWraps writes testdata/wraps.json. The keys and the rumor are fixed
// test values. The seals and wraps are new on each run.
func recordWraps(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	names := []string{"alice", "bob", "carol", "mallory"}
	known := map[string]keys.Key{}
	file := wrapVectors{
		Description: "Gift wraps of core/private, recorded once. A recipient opens each wrap in open, and refuses each wrap or seal in refuse.",
		Keys:        map[string]keyVector{},
	}
	for _, name := range names {
		k := keys.FromSecret(nostr.SecretKey(sha256.Sum256([]byte("arc test vector " + name))))
		known[name] = k
		file.Keys[name] = keyVector{Secret: k.Secret.Hex(), Public: k.Public.Hex()}
	}
	alice, bob, carol, mallory := known["alice"], known["bob"], known["carol"], known["mallory"]

	at := time.Unix(1791115200, 0)
	expires := at.Add(private.MaxAge)
	rumor := private.Rumor(alice, 14, "hello bob", nostr.Tags{{"p", bob.Public.Hex()}}, at)
	seal, err := private.Seal(ctx, alice, bob.Public, rumor)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := bob.Decrypt(ctx, seal.Content, alice.Public)
	if err != nil {
		t.Fatal(err)
	}
	want := rumorVector{ID: rumor.ID.Hex(), PubKey: rumor.PubKey.Hex(), CreatedAt: int64(rumor.CreatedAt), Kind: int(rumor.Kind), Tags: rumor.Tags, Content: rumor.Content}

	for _, form := range []private.Form{private.RelayForm, private.CourierForm} {
		before := time.Now().UTC().Format(time.DateOnly)
		wrap, err := private.WrapSeal(seal, bob.Public, form, private.WrapKind, expires)
		if err != nil {
			t.Fatal(err)
		}
		v := openVector{RecipientSecret: bob.Secret.Hex(), Expires: expires.Unix(), Wrap: wrap, Seal: seal, SealPlaintext: plain, Author: alice.Public.Hex(), Rumor: want}
		if form == private.RelayForm {
			v.Description, v.Form = "a message from alice to bob, in the relay form", "relay"
		} else {
			if after := time.Now().UTC().Format(time.DateOnly); after != before {
				t.Fatal("the UTC day changed while recording; run again")
			}
			v.Description, v.Form, v.RouteDay = "a message from alice to bob, in the courier form", "courier", before
		}
		file.Open = append(file.Open, v)
	}

	// A seal by Mallory around a rumor that claims Alice wrote it.
	forged := private.Rumor(alice, 14, "send money to mallory", nostr.Tags{{"p", bob.Public.Hex()}}, at)
	conv, err := nip44.GenerateConversationKey(bob.Public, mallory.Secret)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := nip44.Encrypt(forged.String(), conv)
	if err != nil {
		t.Fatal(err)
	}
	forgedSeal := nostr.Event{Kind: private.SealKind, CreatedAt: nostr.Timestamp(at.Unix()), Content: inner, Tags: nostr.Tags{}}
	if err := forgedSeal.Sign(mallory.Secret); err != nil {
		t.Fatal(err)
	}
	forgedWrap, err := private.WrapSeal(forgedSeal, bob.Public, private.RelayForm, private.WrapKind, expires)
	if err != nil {
		t.Fatal(err)
	}

	// A seal by Alice whose ciphertext has one byte changed, signed again.
	brokenSeal := nostr.Event{Kind: private.SealKind, CreatedAt: seal.CreatedAt, Content: flipByte(t, seal.Content), Tags: nostr.Tags{}}
	if err := brokenSeal.Sign(alice.Secret); err != nil {
		t.Fatal(err)
	}
	brokenSealWrap, err := private.WrapSeal(brokenSeal, bob.Public, private.RelayForm, private.WrapKind, expires)
	if err != nil {
		t.Fatal(err)
	}

	relayWrap := file.Open[0].Wrap
	brokenWrap := corruptWrap(t, seal, bob.Public, expires)
	file.Refuse = []refuseVector{
		{Description: "a wrap for bob, opened by carol", RecipientSecret: carol.Secret.Hex(), Wrap: &relayWrap, Error: "wrap-does-not-open"},
		{Description: "a signed wrap whose ciphertext has one byte changed", RecipientSecret: bob.Secret.Hex(), Wrap: &brokenWrap, Error: "wrap-does-not-open"},
		{Description: "a wrap around a signed seal whose ciphertext has one byte changed", RecipientSecret: bob.Secret.Hex(), Wrap: &brokenSealWrap, Error: "seal-does-not-open"},
		{Description: "a wrap around a seal by mallory, whose rumor names alice as its author", RecipientSecret: bob.Secret.Hex(), Wrap: &forgedWrap, Error: "rumor-author-mismatch"},
		{Description: "a seal by mallory, whose rumor names alice as its author", RecipientSecret: bob.Secret.Hex(), Seal: &forgedSeal, Error: "rumor-author-mismatch"},
	}

	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/wraps.json", append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

// corruptWrap wraps a seal under a one-time key, changes one byte of the
// ciphertext, and signs the wrap again. The wrap has a valid signature, so
// only the decryption fails.
func corruptWrap(t *testing.T, seal nostr.Event, recipient nostr.PubKey, expires time.Time) nostr.Event {
	t.Helper()
	once := nostr.Generate()
	conv, err := nip44.GenerateConversationKey(recipient, once)
	if err != nil {
		t.Fatal(err)
	}
	content, err := nip44.Encrypt(seal.String(), conv)
	if err != nil {
		t.Fatal(err)
	}
	wrap := nostr.Event{
		Kind:      private.WrapKind,
		CreatedAt: seal.CreatedAt,
		Tags:      nostr.Tags{{"p", recipient.Hex()}, {"expiration", strconv.FormatInt(expires.Unix(), 10)}},
		Content:   flipByte(t, content),
	}
	if err := wrap.Sign(once); err != nil {
		t.Fatal(err)
	}
	return wrap
}

// flipByte changes the first byte of the ciphertext in a NIP-44 payload:
// the byte after the version byte and the 32-byte nonce.
func flipByte(t *testing.T, payload string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	raw[33] ^= 0x01
	return base64.StdEncoding.EncodeToString(raw)
}
