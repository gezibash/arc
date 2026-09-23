package catalog_test

import (
	"testing"

	"github.com/gezibash/arc/delivery/catalog"
)

const hexKey = "c2be57423e79dfa416b0d81401e619254d5874faf3ec96258cb15c001c502be0"

// The rules come from the parser of the older stack, docs/transport/SPEC.md
// of v0.10.0: a lower-case scheme, a resource path of ASCII letters, digits
// and / . _ ~ -, and no user information, port, query, fragment, percent
// escape or dot segment. The provider can now be any form of key.
func TestAnAddressNamesAProviderAndAResource(t *testing.T) {
	for text, want := range map[string]catalog.Address{
		"sqlite+arc://" + hexKey + "/main":      {Scheme: "sqlite", Provider: hexKey, Path: "/main"},
		"exec+arc://" + hexKey + "/":            {Scheme: "exec", Provider: hexKey, Path: "/"},
		"exec+arc://" + hexKey:                  {Scheme: "exec", Provider: hexKey, Path: "/"},
		"exec+arc://npub1xyz/":                  {Scheme: "exec", Provider: "npub1xyz", Path: "/"},
		"releases+arc://scout/releases":         {Scheme: "releases", Provider: "scout", Path: "/releases"},
		"my-db2+arc://lucid-poisson-2cd0f5b7/a": {Scheme: "my-db2", Provider: "lucid-poisson-2cd0f5b7", Path: "/a"},
		"fs+arc://scout/a/b_c.d~e-f":            {Scheme: "fs", Provider: "scout", Path: "/a/b_c.d~e-f"},
	} {
		got, err := catalog.ParseAddress(text)
		if err != nil || got != want {
			t.Errorf("%s: %+v, %v; want %+v", text, got, err, want)
		}
	}
}

func TestAnAddressRefusesWhatTheOlderParserRefused(t *testing.T) {
	for _, text := range []string{
		"exec://" + hexKey + "/",                  // not +arc
		"Exec+arc://" + hexKey + "/",              // upper case scheme
		"ex_ec+arc://" + hexKey + "/",             // scheme character
		"exec+arc://bob@example.com/",             // user information
		"exec+arc://" + hexKey + ":7331/",         // port
		"exec+arc://" + hexKey + "/main?x=1",      // query
		"exec+arc://" + hexKey + "/main#top",      // fragment
		"exec+arc://" + hexKey + "/a%2Fb",         // percent escape
		"exec+arc://" + hexKey + "/../etc/passwd", // dot segment
		"exec+arc://" + hexKey + "/a/./b",         // dot segment
		"exec+arc://" + hexKey + "/a b",           // space
		"exec+arc:///main",                        // no provider
	} {
		if got, err := catalog.ParseAddress(text); err == nil {
			t.Errorf("%s passed as %+v", text, got)
		}
	}
}

func TestAnAddressIsToldApartFromAName(t *testing.T) {
	for text, want := range map[string]bool{
		"exec+arc://" + hexKey + "/": true,
		"scout":                      false,
		hexKey:                       false,
		"exec":                       false,
		"http://example.com/+arc://": false,
	} {
		if got := catalog.IsAddress(text); got != want {
			t.Errorf("IsAddress(%q) = %v, want %v", text, got, want)
		}
	}
}
