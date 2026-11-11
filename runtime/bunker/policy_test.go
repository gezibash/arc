package bunker

import (
	"strconv"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip46"
)

func TestTheBunkerPolicy(t *testing.T) {
	owner, other, device := nostr.Generate().Public(), nostr.Generate().Public(), nostr.Generate().Public()
	token := func(to nostr.PubKey, at nostr.Timestamp, life int64) nip46.Request {
		event := nostr.Event{Kind: 20274, CreatedAt: at, Tags: nostr.Tags{{"p", to.Hex()}, {"expiration", strconv.FormatInt(int64(at)+life, 10)}}}
		body, _ := event.MarshalJSON()
		return nip46.Request{Method: "sign_event", Params: []string{string(body)}}
	}
	now := nostr.Now()
	week := int64(7 * 24 * 60 * 60)
	sign := func(kind int) nip46.Request {
		event := nostr.Event{Kind: nostr.Kind(kind), CreatedAt: 1, Tags: nostr.Tags{}}
		body, _ := event.MarshalJSON()
		return nip46.Request{Method: "sign_event", Params: []string{string(body)}}
	}
	decrypt := func(from nostr.PubKey) nip46.Request {
		return nip46.Request{Method: "nip44_decrypt", Params: []string{from.Hex(), "ciphertext"}}
	}

	cases := []struct {
		name   string
		policy Policy
		req    nip46.Request
		want   string
	}{
		{"an allowed kind", Policy{Kinds: []nostr.Kind{11}, Decrypt: "self"}, sign(11), ""},
		{"a kind not allowed", Policy{Kinds: []nostr.Kind{11}, Decrypt: "self"}, sign(1111), "does not sign kind 1111"},
		{"authentication", Policy{Kinds: []nostr.Kind{11}, Decrypt: "self"}, sign(22242), ""},
		{"any kind", Policy{Decrypt: "self"}, sign(0), ""},
		{"never a device list", Policy{Decrypt: "self"}, sign(10274), "never signs a device list"},
		{"a device list even when allowed", Policy{Kinds: []nostr.Kind{10274}, Decrypt: "self"}, sign(10274), "never signs a device list"},
		{"never a storage list", Policy{Decrypt: "self"}, sign(10273), "never signs a storage list"},
		{"a storage list even when allowed", Policy{Kinds: []nostr.Kind{10273}, Decrypt: "self"}, sign(10273), "never signs a storage list"},
		{"never the data roots", Policy{Kinds: []nostr.Kind{10275}, Decrypt: "self"}, sign(10275), "never signs the data roots"},
		{"a token for the device that asks", Policy{Kinds: []nostr.Kind{11}, Decrypt: "self"}, token(device, now, week), ""},
		{"no token for another key", Policy{Decrypt: "self"}, token(other, now, week), "only for the device that asks"},
		{"no token for longer than 7 days", Policy{Decrypt: "self"}, token(device, now, week+1), "only for the device that asks"},
		{"no token that has ended", Policy{Decrypt: "self"}, token(device, now-nostr.Timestamp(week), week), "only for the device that asks"},
		{"self opens own", Policy{Decrypt: "self"}, decrypt(owner), ""},
		{"self refuses others", Policy{Decrypt: "self"}, decrypt(other), "decrypts only"},
		{"none refuses own", Policy{Decrypt: "none"}, decrypt(owner), "does not decrypt"},
		{"all opens others", Policy{Decrypt: "all"}, decrypt(other), ""},
		{"nip04 too", Policy{Decrypt: "self"}, nip46.Request{Method: "nip04_decrypt", Params: []string{other.Hex(), "x"}}, "decrypts only"},
		{"encrypt is open", Policy{Decrypt: "none"}, nip46.Request{Method: "nip44_encrypt", Params: []string{other.Hex(), "x"}}, ""},
	}
	for _, c := range cases {
		err := c.policy.refusal(c.req, owner, device)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}
