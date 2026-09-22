package release_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gezibash/arc/release"
)

// channelOf signs one channel document at a sequence.
func channelOf(t *testing.T, publisher *publisherKey, sequence int) map[string]any {
	t.Helper()

	unsigned := channelDocument(publisher, releaseEntry("9.9.9", "linux", "amd64"))
	unsigned["sequence"] = sequence

	signed, err := release.Sign(publisher.secret, unsigned)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestTheCheckpointRefusesAnOlderDocument(t *testing.T) {
	publisher, err := newPublisher()
	if err != nil {
		t.Fatal(err)
	}

	checkpoint := &release.Checkpoint{Dir: t.TempDir()}

	// The citizen accepts the document at sequence 7 and remembers it.
	expect, err := checkpoint.Read(publisher.PublicKey, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if expect.LastSequence != 0 {
		t.Fatalf("a channel never read answered sequence %d", expect.LastSequence)
	}

	newer, err := release.Verify(channelOf(t, publisher, 7), expect)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Write(publisher.PublicKey, newer); err != nil {
		t.Fatal(err)
	}

	// An older document is signed, and was real once. It must not be taken.
	expect, err = checkpoint.Read(publisher.PublicKey, "stable")
	if err != nil {
		t.Fatal(err)
	}
	if expect.LastSequence != 7 {
		t.Fatalf("the checkpoint remembered sequence %d, want 7", expect.LastSequence)
	}

	if _, err := release.Verify(channelOf(t, publisher, 6), expect); err != release.ErrOutOfSequence {
		t.Errorf("an older document gave %v, want the sequence error", err)
	}

	// The same sequence with other bytes is a second document, and is refused.
	unsigned := channelDocument(publisher, releaseEntry("9.9.8", "linux", "amd64"))
	unsigned["sequence"] = 7

	again, err := release.Sign(publisher.secret, unsigned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := release.Verify(again, expect); err != release.ErrOutOfSequence {
		t.Errorf("a second document at one sequence gave %v, want the sequence error", err)
	}

	// The document that it already took stays acceptable.
	if _, err := release.Verify(channelOf(t, publisher, 8), expect); err != nil {
		t.Errorf("a newer document was refused: %v", err)
	}
}

func TestEachPublisherAndChannelHoldsItsOwnRecord(t *testing.T) {
	first, _ := newPublisher()
	second, _ := newPublisher()
	checkpoint := &release.Checkpoint{Dir: t.TempDir()}

	verified, err := release.Verify(channelOf(t, first, 9), release.Expect{
		Publisher: first.PublicKey, Channel: "stable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Write(first.PublicKey, verified); err != nil {
		t.Fatal(err)
	}

	for _, one := range []struct {
		name      string
		publisher []byte
		channel   string
	}{
		{"another publisher", second.PublicKey, "stable"},
		{"another channel", first.PublicKey, "beta"},
	} {
		expect, err := checkpoint.Read(one.publisher, one.channel)
		if err != nil {
			t.Fatal(err)
		}
		if expect.LastSequence != 0 {
			t.Errorf("%s read the record of another: %d", one.name, expect.LastSequence)
		}
	}
}

func TestTheCheckpointRefusesADamagedRecord(t *testing.T) {
	publisher, _ := newPublisher()
	dir := t.TempDir()
	checkpoint := &release.Checkpoint{Dir: dir}

	verified, err := release.Verify(channelOf(t, publisher, 3), release.Expect{
		Publisher: publisher.PublicKey, Channel: "stable",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := checkpoint.Write(publisher.PublicKey, verified); err != nil {
		t.Fatal(err)
	}

	var path string
	filepath.Walk(dir, func(one string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			path = one
		}
		return nil
	})
	if path == "" {
		t.Fatal("the checkpoint wrote no file")
	}
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A record that cannot be read is not a reason to accept an old document.
	if _, err := checkpoint.Read(publisher.PublicKey, "stable"); err == nil {
		t.Error("a damaged record read as an empty one")
	}
}

func TestTheCheckpointRefusesAChannelNameThatIsAPath(t *testing.T) {
	publisher, _ := newPublisher()
	checkpoint := &release.Checkpoint{Dir: t.TempDir()}

	for _, name := range []string{"../escape", "a/b", "", "Stable"} {
		if _, err := checkpoint.Read(publisher.PublicKey, name); err == nil {
			t.Errorf("the channel name %q was allowed", name)
		}
	}
}
