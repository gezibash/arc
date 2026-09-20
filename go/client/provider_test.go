package client_test

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/gezibash/arc/go/frame"
	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/packet"
	"github.com/gezibash/arc/go/session"
)

// The Elixir runtime serves the Go exec provider. A Go client calls it over
// the relay, so the provider protocol matches in both implementations.
//
// To run this test: mise run go.provider
func TestGoProviderAnswersOverTheElixirRuntime(t *testing.T) {
	address, relayKey := relay(t)

	providerKey := os.Getenv("ARC_PROVIDER_KEY")
	callerSeed := os.Getenv("ARC_CALLER_SEED")
	if providerKey == "" || callerSeed == "" {
		t.Skip("no provider: run mise run go.provider")
	}

	me, err := identity.FromSeedHex(callerSeed)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := hex.DecodeString(providerKey)
	if err != nil {
		t.Fatal(err)
	}

	connection := dial(t, address, relayKey, me)
	talk, err := session.Establish(me, peer)
	if err != nil {
		t.Fatal(err)
	}

	call := func(body map[string]any) map[string]any {
		t.Helper()

		message, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}

		requestID := frame.NewRequestID()
		request, err := frame.Encode(frame.Request, requestID, map[string]any{
			"method": "EXEC",
			"path":   "/",
		}, message)
		if err != nil {
			t.Fatal(err)
		}

		nonce, ciphertext, seq, err := talk.Encrypt(request)
		if err != nil {
			t.Fatal(err)
		}

		raw, err := packet.Encode(me, peer, talk.ID, seq, nonce, ciphertext,
			packet.WithEphemeralKey(talk.EphemeralPublic))
		if err != nil {
			t.Fatal(err)
		}

		if err := connection.SendPacket(raw); err != nil {
			t.Fatal(err)
		}

		deadline := time.After(30 * time.Second)
		for {
			select {
			case arrived := <-connection.Packets():
				got, err := packet.Decode(arrived)
				if err != nil {
					t.Fatalf("decode: %v", err)
				}

				plaintext, err := talk.Decrypt(got.Nonce, got.Ciphertext)
				if err != nil {
					t.Fatalf("decrypt: %v", err)
				}

				answer, err := frame.Decode(plaintext)
				if err != nil {
					t.Fatalf("frame: %v", err)
				}
				if string(answer.RequestID) != string(requestID) {
					continue
				}
				if answer.Type == frame.Error {
					t.Fatalf("the provider answered %s: %s", answer.Code(), answer.Message())
				}

				var reply map[string]any
				if err := json.Unmarshal(answer.Body, &reply); err != nil {
					t.Fatalf("the reply is not JSON: %q", answer.Body)
				}
				return reply

			case <-deadline:
				t.Fatal("the provider did not answer")
			case <-connection.Done():
				t.Fatalf("the connection ended: %v", connection.Err())
			}
		}
	}

	result := call(map[string]any{"argv": []string{"echo", "hello from go"}})
	if result["stdout"] != "hello from go\n" || result["exit"] != float64(0) {
		t.Errorf("run = %v", result)
	}

	started := call(map[string]any{"action": "start", "script": "echo job output"})
	job, _ := started["job"].(string)
	if job == "" || started["state"] != "running" {
		t.Fatalf("start = %v", started)
	}

	var status map[string]any
	for attempt := 0; attempt < 50; attempt++ {
		status = call(map[string]any{"action": "status", "job": job})
		if status["state"] == "done" {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if status["state"] != "done" || status["stdout"] != "job output\n" {
		t.Errorf("status = %v", status)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := connection.Status(ctx); err != nil {
		t.Errorf("the connection did not survive the calls: %v", err)
	}
}
