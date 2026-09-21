package client_test

import (
	"strings"
	"testing"

	"github.com/gezibash/arc/client"
)

var key = strings.Repeat("ab", 32)

func TestParseAddressTakesAPlainPath(t *testing.T) {
	for path, want := range map[string]string{
		"":              "/",
		"/":             "/",
		"/main":         "/main",
		"/releases":     "/releases",
		"/a/b-c_d.e~f/": "/a/b-c_d.e~f/",
		"/.hidden":      "/.hidden",
		"/informal":     "/informal",
	} {
		address, err := client.ParseAddress("sqlite+arc://" + key + path)
		if err != nil {
			t.Errorf("%q: %v", path, err)
			continue
		}
		if address.Path != want || address.Scheme != "sqlite" {
			t.Errorf("%q: got %+v", path, address)
		}
	}
}

// A path that could name something other than what it shows is refused.
func TestParseAddressRefusesAPathThatHides(t *testing.T) {
	for _, path := range []string{
		"/a/../b",
		"/..",
		"/./main",
		"/a/.",
		"/%2e%2e",
		"/a%20b",
		"/main%2Fother",
		"/info",
		"/info/capabilities/primary",
		"/a b",
		"/a;b",
		"/ümlaut",
	} {
		if _, err := client.ParseAddress("sqlite+arc://" + key + path); err == nil {
			t.Errorf("%q passed", path)
		}
	}
}

func TestParseAddressRefusesWhatIsNotAnAddress(t *testing.T) {
	for _, raw := range []string{
		"sqlite://" + key + "/main",
		"sqlite+arc://" + key[:10] + "/main",
		"sqlite+arc://user@" + key + "/main",
		"sqlite+arc://" + key + ":7331/main",
		"sqlite+arc://" + key + "/main?x=1",
		"sqlite+arc://" + key + "/main#top",
	} {
		if _, err := client.ParseAddress(raw); err == nil {
			t.Errorf("%q passed", raw)
		}
	}
}
