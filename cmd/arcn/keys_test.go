package main

import (
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip46"
)

func TestTheBunkerPolicy(t *testing.T) {
	owner, other := nostr.Generate().Public(), nostr.Generate().Public()
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
		policy policy
		req    nip46.Request
		want   string
	}{
		{"an allowed kind", policy{kinds: []nostr.Kind{11}, decrypt: "self"}, sign(11), ""},
		{"a kind not allowed", policy{kinds: []nostr.Kind{11}, decrypt: "self"}, sign(1111), "does not sign kind 1111"},
		{"authentication", policy{kinds: []nostr.Kind{11}, decrypt: "self"}, sign(22242), ""},
		{"any kind", policy{decrypt: "self"}, sign(0), ""},
		{"self opens own", policy{decrypt: "self"}, decrypt(owner), ""},
		{"self refuses others", policy{decrypt: "self"}, decrypt(other), "decrypts only"},
		{"none refuses own", policy{decrypt: "none"}, decrypt(owner), "does not decrypt"},
		{"all opens others", policy{decrypt: "all"}, decrypt(other), ""},
		{"nip04 too", policy{decrypt: "self"}, nip46.Request{Method: "nip04_decrypt", Params: []string{other.Hex(), "x"}}, "decrypts only"},
		{"encrypt is open", policy{decrypt: "none"}, nip46.Request{Method: "nip44_encrypt", Params: []string{other.Hex(), "x"}}, ""},
	}
	for _, c := range cases {
		err := c.policy.refusal(c.req, owner)
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: refused: %v", c.name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}
