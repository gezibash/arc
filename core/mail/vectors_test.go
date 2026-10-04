package mail_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/adapters/mailbox"
	boltstore "github.com/gezibash/arc/adapters/store/bolt"
	"github.com/gezibash/arc/core/keys"
	"github.com/gezibash/arc/core/mail"
	"github.com/gezibash/arc/core/node"
	"github.com/gezibash/arc/core/private"
	"github.com/gezibash/arc/core/transport"
)

// testdata/ack_wraps.json is recorded by this test, because a seal and a
// wrap use random keys:
//
//	go test ./core/mail -run '^TestAckRumorVectors$' -update
var update = flag.Bool("update", false, "record testdata/ack_wraps.json again")

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

type ackRumor struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      nostr.Tags `json:"tags"`
	Content   string     `json:"content"`
}

// ackVectors is testdata/ack_rumor.json, from an independent computation.
type ackVectors struct {
	Message struct {
		SenderSecret    string `json:"sender_secret"`
		RecipientSecret string `json:"recipient_secret"`
		Text            string `json:"text"`
		Unix            int64  `json:"unix"`
		ID              string `json:"id"`
	} `json:"message"`
	Vectors []struct {
		Description  string   `json:"description"`
		AuthorSecret string   `json:"author_secret"`
		Unix         int64    `json:"unix"`
		Rumor        ackRumor `json:"rumor"`
		Serialized   string   `json:"serialized"`
		Delivers     bool     `json:"delivers"`
	} `json:"vectors"`
}

// ackWrap is one vector of testdata/ack_wraps.json.
type ackWrap struct {
	Description  string      `json:"description"`
	SenderSecret string      `json:"sender_secret"`
	Wrap         nostr.Event `json:"wrap"`
	Rumor        ackRumor    `json:"rumor"`
	Acknowledges string      `json:"acknowledges"`
	Delivers     bool        `json:"delivers"`
}

type ackWraps struct {
	Description string    `json:"description"`
	Vectors     []ackWrap `json:"vectors"`
}

func readJSON(t *testing.T, path string, into any) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the vectors: %v", err)
	}
	if err := json.Unmarshal(body, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func readAcks(t *testing.T) ackVectors {
	t.Helper()
	var acks ackVectors
	readJSON(t, "testdata/ack_rumor.json", &acks)
	if len(acks.Vectors) == 0 || acks.Message.ID == "" {
		t.Fatal("testdata/ack_rumor.json holds no vectors")
	}
	return acks
}

func mustKey(t *testing.T, secret string) keys.Key {
	t.Helper()
	k, err := keys.Parse(secret)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func sameAck(got nostr.Event, want ackRumor) bool {
	if got.ID.Hex() != want.ID || got.PubKey.Hex() != want.PubKey || int64(got.CreatedAt) != want.CreatedAt ||
		int(got.Kind) != want.Kind || got.Content != want.Content || len(got.Tags) != len(want.Tags) {
		return false
	}
	for i := range got.Tags {
		if strings.Join(got.Tags[i], "\x00") != strings.Join(want.Tags[i], "\x00") {
			return false
		}
	}
	return true
}

// sendMessage makes the mail of the sender, with the message of the vectors
// in its outbox.
func sendMessage(t *testing.T, acks ackVectors) (*mail.Mail, keys.Key) {
	t.Helper()
	alice, bob := mustKey(t, acks.Message.SenderSecret), mustKey(t, acks.Message.RecipientSecret)
	m := openMail(t, alice)
	mail.SetNow(m, func() time.Time { return time.Unix(acks.Message.Unix, 0) })
	out, err := m.Send(context.Background(), bob.Public, acks.Message.Text)
	if err != nil || out.Rumor != acks.Message.ID {
		t.Fatalf("the outbox names rumor %s, want %s (%v)", out.Rumor, acks.Message.ID, err)
	}
	return m, alice
}

func outboxState(t *testing.T, m *mail.Mail) string {
	t.Helper()
	out, err := m.Outbox(context.Background())
	if err != nil || len(out) != 1 {
		t.Fatalf("outbox %+v, %v", out, err)
	}
	return out[0].State(time.Unix(out[0].Created.Unix(), 0))
}

// The recipient receives the message, and makes the acknowledgement of the
// vectors. The sender opens it, and the message shows as delivered.
func TestAckRumorVectors(t *testing.T) {
	acks := readAcks(t)
	want := acks.Vectors[0]
	if !want.Delivers {
		t.Fatal("the first vector must be the acknowledgement that delivers")
	}
	sum := sha256.Sum256([]byte(want.Serialized))
	if hex.EncodeToString(sum[:]) != want.Rumor.ID {
		t.Fatal("the vector is broken: its id is not the hash of its serialized form")
	}

	ctx := context.Background()
	sender, alice := sendMessage(t, acks)
	bob := openMail(t, mustKey(t, acks.Message.RecipientSecret))
	mail.SetNow(bob, func() time.Time { return time.Unix(want.Unix, 0) })
	carrier := &memory{}

	if _, err := sender.Sync(ctx, carrier); err != nil {
		t.Fatal(err)
	}
	if got, err := bob.Sync(ctx, carrier); err != nil || got.Received != 1 {
		t.Fatalf("bob received %d messages (%v)", got.Received, err)
	}
	if _, err := bob.Sync(ctx, carrier); err != nil {
		t.Fatal(err)
	}

	ack := findAck(t, carrier, alice)
	if !sameAck(ack.rumor, want.Rumor) {
		t.Errorf("acknowledgement %s, want %+v", ack.rumor, want.Rumor)
	}
	if string(ack.rumor.Serialize()) != want.Serialized {
		t.Errorf("serialized\n%s\nwant\n%s", ack.rumor.Serialize(), want.Serialized)
	}

	mail.SetNow(sender, func() time.Time { return time.Unix(want.Unix, 0) })
	if got, err := sender.Sync(ctx, carrier); err != nil || got.Delivered != 1 {
		t.Fatalf("alice saw %d acknowledgements (%v)", got.Delivered, err)
	}
	if state := outboxState(t, sender); state != "delivered" {
		t.Errorf("the message is %s", state)
	}

	if *update {
		recordAcks(t, acks, ack.wrap)
	}
}

type foundAck struct {
	wrap  nostr.Event
	rumor nostr.Event
}

// memory is a transport that holds events in memory, as a relay does. It
// takes the relay form of a wrap, which a carrier does not take.
type memory struct{ events []nostr.Event }

func (m *memory) Name() string { return "memory" }

func (m *memory) Send(_ context.Context, event nostr.Event) error {
	m.events = append(m.events, event)
	return nil
}

func (m *memory) Fetch(_ context.Context, filter nostr.Filter) (transport.Batch, error) {
	var batch transport.Batch
	for _, event := range m.events {
		if filter.Matches(event) {
			batch.Events = append(batch.Events, event)
		}
	}
	return batch, nil
}

// findAck opens each wrap on the carrier with the key of the sender, and
// returns the relay-form wrap of the acknowledgement.
func findAck(t *testing.T, carrier transport.Transport, me keys.Key) foundAck {
	t.Helper()
	batch, err := carrier.Fetch(context.Background(), nostr.Filter{Kinds: []nostr.Kind{private.WrapKind}})
	if err != nil {
		t.Fatal(err)
	}
	for _, wrap := range batch.Events {
		if p := wrap.Tags.Find("p"); p == nil || p[1] != me.Public.Hex() {
			continue
		}
		opened, err := private.Unwrap(context.Background(), me, wrap)
		if err == nil && opened.Rumor.Kind == mail.AckKind {
			return foundAck{wrap: wrap, rumor: opened.Rumor}
		}
	}
	t.Fatal("no acknowledgement reached the carrier")
	return foundAck{}
}

// The sender opens each recorded acknowledgement. Only the acknowledgement
// by the recipient, with an e tag, marks the message delivered.
func TestAckWrapVectors(t *testing.T) {
	acks := readAcks(t)
	var file ackWraps
	readJSON(t, "testdata/ack_wraps.json", &file)
	if len(file.Vectors) != len(acks.Vectors) {
		t.Fatalf("testdata/ack_wraps.json holds %d vectors, want %d", len(file.Vectors), len(acks.Vectors))
	}

	for i, v := range file.Vectors {
		t.Run(v.Description, func(t *testing.T) {
			ctx := context.Background()
			want := acks.Vectors[i]
			if !sameRumor(v.Rumor, want.Rumor) || v.Delivers != want.Delivers {
				t.Fatalf("the vector does not match testdata/ack_rumor.json: %+v", v)
			}

			sender, alice := sendMessage(t, acks)
			if v.SenderSecret != acks.Message.SenderSecret {
				t.Fatal("the vector names another sender")
			}
			opened, err := private.Unwrap(ctx, alice, v.Wrap)
			if err != nil {
				t.Fatal(err)
			}
			if !sameAck(opened.Rumor, want.Rumor) {
				t.Errorf("acknowledgement %s, want %+v", opened.Rumor, want.Rumor)
			}
			acknowledges := ""
			if e := opened.Rumor.Tags.Find("e"); len(e) > 1 {
				acknowledges = e[1]
			}
			if acknowledges != v.Acknowledges {
				t.Errorf("the acknowledgement names %q, want %q", acknowledges, v.Acknowledges)
			}

			carrier := &memory{}
			if err := carrier.Send(ctx, v.Wrap); err != nil {
				t.Fatal(err)
			}
			mail.SetNow(sender, func() time.Time { return time.Unix(want.Unix, 0) })
			got, err := sender.Sync(ctx, carrier)
			if err != nil {
				t.Fatal(err)
			}
			delivered := outboxState(t, sender) == "delivered"
			if delivered != v.Delivers || (got.Delivered == 1) != v.Delivers {
				t.Errorf("delivered %t (%d in the report), want %t", delivered, got.Delivered, v.Delivers)
			}
		})
	}
}

func sameRumor(a, b ackRumor) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// recordAcks writes testdata/ack_wraps.json. The first wrap is the one that
// the recipient made in TestAckRumorVectors. The other wraps hold the
// rumors of testdata/ack_rumor.json that must not deliver.
func recordAcks(t *testing.T, acks ackVectors, made nostr.Event) {
	t.Helper()
	alice := mustKey(t, acks.Message.SenderSecret)
	file := ackWraps{Description: "Gift wraps of acknowledgements to the sender of the message in ack_rumor.json, recorded once. The sender opens each wrap."}
	for i, v := range acks.Vectors {
		wrap := made
		if i > 0 {
			author := mustKey(t, v.AuthorSecret)
			rumor := private.Rumor(author, mail.AckKind, v.Rumor.Content, v.Rumor.Tags, time.Unix(v.Unix, 0))
			var err error
			wrap, err = private.Wrap(context.Background(), author, alice.Public, rumor, private.RelayForm, private.WrapKind, time.Unix(v.Unix, 0).Add(private.MaxAge))
			if err != nil {
				t.Fatal(err)
			}
		}
		acknowledges := ""
		if e := v.Rumor.Tags.Find("e"); len(e) > 1 {
			acknowledges = e[1]
		}
		file.Vectors = append(file.Vectors, ackWrap{Description: v.Description, SenderSecret: acks.Message.SenderSecret, Wrap: wrap, Rumor: v.Rumor, Acknowledges: acknowledges, Delivers: v.Delivers})
	}
	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("testdata/ack_wraps.json", append(body, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
