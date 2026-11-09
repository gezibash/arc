package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/keys"
)

// TestTheBunkerRefusesHolderRequests sends each holder request to the
// bunker as a device does, over NIP-46. The bunker refuses each one: an
// account key or a release signature would give a device the powers of the
// owner (docs/devices/SPEC.md, section 3.7).
func TestTheBunkerRefusesHolderRequests(t *testing.T) {
	ctx := context.Background()
	owner, device := keys.Generate(), keys.Generate()
	asked := 0
	signer := keys.Remote{Public: owner.Public, Call: func(ctx context.Context, method string, params ...string) (string, error) {
		if keys.HolderMethod(method) {
			asked++
		}
		r := owner.Answer(ctx, keys.Request{ID: "1", Method: method, Params: params})
		if r.Error != "" {
			return "", errors.New(r.Error)
		}
		return r.Result, nil
	}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	for _, c := range []struct {
		method string
		params []string
	}{
		{keys.MethodAccountKey, []string{keys.Generate().Public.Hex()}},
		{keys.MethodSignRelease, []string{"1", "00"}},
		{keys.MethodKeyedRoot, nil},
		{keys.MethodSignJoin, []string{keys.Generate().Public.Hex(), keys.Generate().Public.Hex()}},
		{keys.MethodSignChange, []string{"blob", strings.Repeat("00", 32)}},
	} {
		body, _ := json.Marshal(map[string]any{"id": c.method, "method": c.method, "params": c.params})
		sealed, err := device.Encrypt(ctx, string(body), owner.Public)
		if err != nil {
			t.Fatal(err)
		}
		request := nostr.Event{Kind: nostr.KindNostrConnect, CreatedAt: nostr.Now(), Content: sealed, Tags: nostr.Tags{{"p", owner.Public.Hex()}}}
		if err := request.Sign(device.Secret); err != nil {
			t.Fatal(err)
		}
		response, ok := answer(ctx, signer, request, policy{decrypt: "all"}, func(nostr.PubKey, string) bool { return true }, log)
		if !ok {
			t.Fatalf("%s: the bunker gave no answer", c.method)
		}
		plain, err := device.Decrypt(ctx, response.Content, owner.Public)
		if err != nil {
			t.Fatal(err)
		}
		var r keys.Response
		if err := json.Unmarshal([]byte(plain), &r); err != nil {
			t.Fatal(err)
		}
		if r.Result != "" || !strings.Contains(r.Error, "holder request") {
			t.Errorf("%s: the bunker answered %+v, want a refusal", c.method, r)
		}
	}
	if asked != 0 {
		t.Fatalf("the bunker sent %d holder requests to the signer", asked)
	}
}
