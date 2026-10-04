package mail_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/mailbox"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
)

// The vectors in testdata/message_rumor.json come from an independent
// computation, not from this package. testdata/README.md documents them.
type messageVector struct {
	Description  string `json:"description"`
	SenderSecret string `json:"sender_secret"`
	Recipient    string `json:"recipient"`
	Text         string `json:"text"`
	Unix         int64  `json:"unix"`
	Rumor        struct {
		ID        string     `json:"id"`
		PubKey    string     `json:"pubkey"`
		CreatedAt int64      `json:"created_at"`
		Kind      int        `json:"kind"`
		Tags      nostr.Tags `json:"tags"`
		Content   string     `json:"content"`
	} `json:"rumor"`
	Serialized string `json:"serialized"`
}

func TestMessageRumorVectors(t *testing.T) {
	body, err := os.ReadFile("testdata/message_rumor.json")
	if err != nil {
		t.Fatalf("read the vectors: %v", err)
	}
	var file struct {
		Vectors []messageVector `json:"vectors"`
	}
	if err := json.Unmarshal(body, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("testdata/message_rumor.json holds no vectors")
	}

	for _, v := range file.Vectors {
		t.Run(v.Description, func(t *testing.T) {
			sum := sha256.Sum256([]byte(v.Serialized))
			if hex.EncodeToString(sum[:]) != v.Rumor.ID {
				t.Fatal("the vector is broken: its id is not the hash of its serialized form")
			}

			sender, err := keys.Parse(v.SenderSecret)
			if err != nil {
				t.Fatal(err)
			}
			recipient, err := nostr.PubKeyFromHex(v.Recipient)
			if err != nil {
				t.Fatal(err)
			}
			m := openMail(t, sender)
			mail.SetNow(m, func() time.Time { return time.Unix(v.Unix, 0) })

			out, err := m.Send(context.Background(), recipient, v.Text)
			if err != nil {
				t.Fatal(err)
			}
			if out.Rumor != v.Rumor.ID {
				t.Errorf("the outbox names rumor %s, want %s", out.Rumor, v.Rumor.ID)
			}

			rumors, err := m.Rumors(context.Background(), []nostr.Kind{mail.MessageKind})
			if err != nil || len(rumors) != 1 {
				t.Fatalf("rumors %v, %v", rumors, err)
			}
			got := rumors[0]
			if string(got.Serialize()) != v.Serialized {
				t.Errorf("serialized\n%s\nwant\n%s", got.Serialize(), v.Serialized)
			}
			want := v.Rumor
			if got.ID.Hex() != want.ID || got.PubKey.Hex() != want.PubKey || int64(got.CreatedAt) != want.CreatedAt ||
				int(got.Kind) != want.Kind || got.Content != want.Content ||
				len(got.Tags) != 1 || len(got.Tags[0]) != 2 || got.Tags[0][0] != want.Tags[0][0] || got.Tags[0][1] != want.Tags[0][1] {
				t.Errorf("rumor %s, want %+v", got, want)
			}
		})
	}
}

// openMail opens the mail of a given key, with no relays.
func openMail(t *testing.T, k keys.Key) *mail.Mail {
	t.Helper()
	dir := t.TempDir()
	s, err := boltstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	m, err := mailbox.Open(dir, k, &node.Node{Store: s}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Close() })
	return m
}
