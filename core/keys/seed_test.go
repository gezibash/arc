package keys

import (
	"strings"
	"testing"
)

// The test vectors of NIP-06, copied from
// https://github.com/nostr-protocol/nips/blob/master/06.md.
var nip06Vectors = []struct{ words, secret, public string }{
	{
		"leader monkey parrot ring guide accident before fence cannon height naive bean",
		"7f7ff03d123792d6ac594bfa67bf6d0c0ab55b6b1fdb6249303fe861f1ccba9a",
		"17162c921dc4d2518f9a101db33695df1afb56ab82f5ff3e5da6eec3ca5cd917",
	},
	{
		"what bleak badge arrange retreat wolf trade produce cricket blur garlic valid proud rude strong choose busy staff weather area salt hollow arm fade",
		"c15d739894c81a2fcfd3a2df85a0d2c0dbc47a280d092799f144d73d7ae78add",
		"d41b22899549e1f3d335a31002cfd382174006e166d3e658e3a5eecdb6463573",
	},
}

func TestFromSeedGivesTheKeysOfNIP06(t *testing.T) {
	for _, v := range nip06Vectors {
		k, err := FromSeed(v.words, "")
		if err != nil {
			t.Fatalf("%q: %v", v.words, err)
		}
		if k.Secret.Hex() != v.secret || k.Public.Hex() != v.public {
			t.Fatalf("%q gave\nsecret %s\npublic %s\nwant\nsecret %s\npublic %s",
				v.words, k.Secret.Hex(), k.Public.Hex(), v.secret, v.public)
		}
	}
}

// Words typed by a person carry other spaces, a newline or capitals. They
// must give the key of the words, not a key of the spacing.
func TestFromSeedIgnoresSpacesAndCase(t *testing.T) {
	v := nip06Vectors[1]
	typed := "  " + strings.ToUpper(strings.ReplaceAll(v.words, " ", "  \t")) + "\n"
	k, err := FromSeed(typed, "")
	if err != nil {
		t.Fatal(err)
	}
	if k.Secret.Hex() != v.secret {
		t.Fatalf("the typed words gave another key: %s", k.Public.Hex())
	}
}

func TestFromSeedRefusesBadWords(t *testing.T) {
	words := strings.Fields(nip06Vectors[1].words)
	last := len(words) - 1
	cases := map[string]string{
		// "fade" is replaced by "face", another word of the list, so only the
		// checksum is wrong.
		"bad checksum":   strings.Join(append(words[:last:last], "face"), " "),
		"unknown word":   strings.Join(append(words[:last:last], "fadex"), " "),
		"18 words":       strings.Join(words[:18], " "),
		"one word short": strings.Join(words[:last], " "),
	}
	for name, text := range cases {
		if _, err := FromSeed(text, ""); err == nil {
			t.Errorf("%s: FromSeed took %q", name, text)
		}
	}
}

func TestANewSeedHas24WordsAndDerivesOneKey(t *testing.T) {
	words, err := NewSeed()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Fields(words)); n != 24 {
		t.Fatalf("a new seed has %d words", n)
	}
	first, err := FromSeed(words, "")
	if err != nil {
		t.Fatal(err)
	}
	again, _ := FromSeed(words, "")
	if first.Secret != again.Secret {
		t.Fatal("the same words gave two keys")
	}
	other, err := FromSeed(words, "trip")
	if err != nil {
		t.Fatal(err)
	}
	if other.Secret == first.Secret {
		t.Fatal("a passphrase did not change the key")
	}
}
