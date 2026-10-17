package files

import (
	"strings"
	"testing"
)

// The name in a link comes from another citizen. It must not put the file
// in another directory.
func TestTheNameOfALinkHasNoDirectory(t *testing.T) {
	key, sum := strings.Repeat("a", 64), strings.Repeat("b", 64)
	for name, want := range map[string]string{
		"bird.png":           "bird.png",
		"../../etc/passwd":   "passwd",
		`..\..\secret.txt`:   "secret.txt",
		"/absolute/path.bin": "path.bin",
		"..":                 sum[:16],
		"":                   sum[:16],
	} {
		_, offer, err := ParseLink(Link(key, Offer{SHA256: sum, Name: name, Size: 1}))
		if err != nil {
			t.Fatalf("%q: %v", name, err)
		}
		if offer.Name != want {
			t.Errorf("the name %q became %q, want %q", name, offer.Name, want)
		}
	}
}

// A name that could change a terminal or hide text loses those characters,
// and newlines and tabs, before the receiver writes the file. A name with
// nothing left becomes the start of the SHA-256.
func TestTheNameOfALinkCannotReachTheTerminal(t *testing.T) {
	key, sum := strings.Repeat("a", 64), strings.Repeat("b", 64)
	england := "\U0001f3f4\U000e0067\U000e0062\U000e0065\U000e006e\U000e0067\U000e007f"
	for _, c := range []struct {
		name, want string
		renamed    bool
	}{
		{"a\x1b[2Jb\u202ec\u200bd\nx.txt", "a[2Jbcdx.txt", true},
		{"x\ty.txt", "xy.txt", true},
		{"\u200b", sum[:16], true},
		{".\u200b.", sum[:16], true},
		{"-rf", "_-rf", true},
		{"\u3164.txt", strings.Repeat("b", 16) + ".txt", true},
		{"\u200b.tar.gz", strings.Repeat("b", 16) + ".tar.gz", true},
		{"\x1b.ssh.authorized_keys", strings.Repeat("b", 16) + ".ssh.authorized_keys", true},
		{".bashrc", ".bashrc", false},
		{"bird.png", "bird.png", false},
		{"caf\u00e9.txt", "caf\u00e9.txt", false},
		{england + ".png", england + ".png", false},
	} {
		_, offer, err := ParseLink(Link(key, Offer{SHA256: sum, Name: c.name, Size: 1}))
		if err != nil {
			t.Fatalf("%q: %v", c.name, err)
		}
		if offer.Name != c.want || offer.Renamed != c.renamed {
			t.Errorf("the name %q became %q, renamed %v; want %q, renamed %v", c.name, offer.Name, offer.Renamed, c.want, c.renamed)
		}
	}
}
