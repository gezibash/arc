package direct

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
