package proof

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestSeed proves the paper backup of a citizen through the normal CLI.
// arc keys gen shows 24 words once and stores none of them. arc keys recover
// makes the same key from the words on another machine. A seed passphrase
// gives another key, and a bad checksum is refused.
func TestSeed(t *testing.T) {
	work := t.TempDir()
	first := arc(t, filepath.Join(work, "first"))

	made := first.try("", "keys", "gen")
	if made.Err != nil {
		t.Fatalf("arc keys gen: %v\n%s", made.Err, made.Stderr)
	}
	words := seedWords(made.Stderr)
	if len(words) != 24 {
		t.Fatalf("arc keys gen showed %d words:\n%s", len(words), made.Stderr)
	}
	lines := strings.Split(strings.TrimSpace(made.Stdout), "\n")
	public := lines[len(lines)-1]
	// Scripts read the name and the public key from standard output.
	if len(lines) != 2 || len(seedWords(made.Stdout)) != 0 {
		t.Fatalf("standard output holds more than the name and the key:\n%s", made.Stdout)
	}
	phrase := strings.Join(words, " ")
	filepath.WalkDir(first.home, func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		body, _ := os.ReadFile(path)
		if strings.Contains(string(body), phrase) || strings.Contains(string(body), words[0]+"\n") {
			t.Fatalf("%s holds the words", path)
		}
		return nil
	})
	t.Log("ok: keys gen shows 24 words on standard error, and stores none")

	second := arc(t, filepath.Join(work, "second"))
	again := strings.Split(strings.TrimSpace(second.input(phrase+"\n", "keys", "recover")), "\n")
	if got := again[len(again)-1]; got != public {
		t.Fatalf("keys recover made %s, keys gen made %s", got, public)
	}
	if whoami := second.run("whoami"); !strings.Contains(whoami, public) {
		t.Fatalf("whoami after recover:\n%s", whoami)
	}
	t.Log("ok: keys recover makes the key of keys gen on another machine")

	third := arc(t, filepath.Join(work, "third")).with("ARC_SEED_PASSPHRASE=trip")
	other := third.input(phrase, "keys", "recover", "--seed-passphrase")
	if strings.Contains(other, public) {
		t.Fatal("the seed passphrase did not change the key")
	}
	t.Log("ok: a seed passphrase gives another key")

	// "abandon" twelve times has a bad checksum; each word is in the list.
	fourth := arc(t, filepath.Join(work, "fourth"))
	bad := fourth.try(strings.Repeat("abandon ", 12), "keys", "recover")
	if bad.Err == nil || !strings.Contains(bad.Stderr, "checksum") {
		t.Fatalf("a bad checksum was not refused: %v\n%s%s", bad.Err, bad.Stdout, bad.Stderr)
	}
	if entries, _ := os.ReadDir(filepath.Join(fourth.home, "citizens")); len(entries) != 0 {
		t.Fatalf("a refused seed left %d entries", len(entries))
	}
	if argv := fourth.try("", append([]string{"keys", "recover"}, words...)...); argv.Err == nil {
		t.Fatal("keys recover took the words as arguments")
	}
	t.Log("ok: a bad checksum and words as arguments are refused")
}

var seedLine = regexp.MustCompile(`(?m)^\s*\d+\. ([a-z]+)$`)

// seedWords reads the numbered words that arc keys gen shows.
func seedWords(text string) []string {
	var words []string
	for _, match := range seedLine.FindAllStringSubmatch(text, -1) {
		words = append(words, match[1])
	}
	return words
}
