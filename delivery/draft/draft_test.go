package draft_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/keyer"
	"fiatjaf.com/nostr/nip44"
	"fiatjaf.com/nostr/nip70"
	"github.com/gezibash/arc/delivery/draft"
)

var ctx = context.Background()

func article(content string) nostr.Event {
	return nostr.Event{Kind: 30023, CreatedAt: 1000, Content: content,
		Tags: nostr.Tags{{"d", "hrs/ablations/lr-sweep"}, {"title", "LR sweep"}}}
}

// A NIP-37 client opens the wrap with nothing but NIP-44 and NIP-01.
func TestAWrapOpensInANIP37Client(t *testing.T) {
	secret := nostr.Generate()
	k := keyer.NewPlainKeySigner(secret)
	wrap, err := draft.Wrap(ctx, k, "some-id", article("auc 0.871\n"), 2000)
	if err != nil {
		t.Fatal(err)
	}

	if wrap.Kind != 31234 || wrap.Tags.GetD() != "some-id" || !wrap.VerifySignature() || !nip70.IsProtected(wrap) {
		t.Fatalf("the wrap is %+v", wrap)
	}
	if k := wrap.Tags.Find("k"); k == nil || k[1] != "30023" {
		t.Errorf("the k tag is %v", k)
	}

	conversation, err := nip44.GenerateConversationKey(secret.Public(), secret)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := nip44.Decrypt(wrap.Content, conversation)
	if err != nil {
		t.Fatal(err)
	}
	var inner nostr.Event
	if err := json.Unmarshal([]byte(plain), &inner); err != nil {
		t.Fatal(err)
	}
	if inner.Kind != 30023 || inner.Content != "auc 0.871\n" || inner.Tags.GetD() != "hrs/ablations/lr-sweep" ||
		inner.PubKey != secret.Public() || inner.ID != inner.GetID() {
		t.Errorf("the client read %+v", inner)
	}
}

func TestOpenAndCheckpoint(t *testing.T) {
	k := keyer.NewPlainKeySigner(nostr.Generate())
	wrap, _ := draft.Wrap(ctx, k, "x", article("v1"), 2000)
	opened, err := draft.Open(ctx, k, wrap)
	if err != nil || opened.D != "x" || opened.Event.Content != "v1" || opened.Deleted {
		t.Fatalf("open: %+v %v", opened, err)
	}

	checkpoint, _ := draft.Checkpoint(ctx, k, "x", article("v1"), 2000)
	me, _ := k.GetPublicKey(ctx)
	if a := checkpoint.Tags.Find("a"); a == nil || a[1] != draft.Coordinate(me, "x") || !nip70.IsProtected(checkpoint) {
		t.Errorf("the checkpoint is %+v", checkpoint)
	}
	back, err := draft.Open(ctx, k, checkpoint)
	if err != nil || back.D != "x" || back.Event.ID != opened.Event.ID {
		t.Errorf("the checkpoint opens as %+v %v", back, err)
	}

	blank, _ := draft.Blank(ctx, k, "x", 30023, 3000)
	if gone, err := draft.Open(ctx, k, blank); err != nil || !gone.Deleted {
		t.Errorf("a blank wrap is not deleted: %+v %v", gone, err)
	}
}

func TestOnlyTheAuthorOpens(t *testing.T) {
	k := keyer.NewPlainKeySigner(nostr.Generate())
	wrap, _ := draft.Wrap(ctx, k, "x", article("secret"), 2000)
	if _, err := draft.Open(ctx, keyer.NewPlainKeySigner(nostr.Generate()), wrap); err == nil {
		t.Error("another citizen opened the draft")
	}
}

func TestSplitCutsAtLineEnds(t *testing.T) {
	line := strings.Repeat("a", 99) + "\n"
	text := strings.Repeat(line, 1000) // 100,000 bytes
	pieces := draft.Split(text)
	if strings.Join(pieces, "") != text {
		t.Fatal("the pieces do not join to the text")
	}
	for i, p := range pieces {
		if len(p) > draft.PartSize || (i < len(pieces)-1 && !strings.HasSuffix(p, "\n")) {
			t.Errorf("piece %d has %d bytes and ends %q", i, len(p), p[len(p)-1:])
		}
	}

	// A long line of multi-byte characters is cut between characters.
	long := strings.Repeat("é", draft.PartSize)
	for _, p := range draft.Split(long) {
		if !strings.HasPrefix(p, "é") || len(p) > draft.PartSize {
			t.Errorf("a piece starts inside a character, or is too long: %d", len(p))
		}
	}
	if got := draft.Split(""); len(got) != 1 || got[0] != "" {
		t.Errorf("empty text splits as %q", got)
	}
}

func TestPartsAndRelayList(t *testing.T) {
	k := keyer.NewPlainKeySigner(nostr.Generate())
	part, _ := draft.Part(ctx, k, "x", "tail text", 2000)
	if text, err := draft.OpenPart(ctx, k, part); err != nil || text != "tail text" || !nip70.IsProtected(part) {
		t.Errorf("the part opens as %q %v", text, err)
	}

	list, _ := draft.RelayList(ctx, k, []string{"wss://a.example", "wss://b.example"}, 2000)
	if strings.Contains(list.Content, "a.example") {
		t.Error("the relay list is not private")
	}
	urls, err := draft.ReadRelayList(ctx, k, list)
	if err != nil || len(urls) != 2 || urls[1] != "wss://b.example" {
		t.Errorf("the relay list reads %v %v", urls, err)
	}
}
