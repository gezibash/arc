package main

import (
	"strings"
	"testing"
)

// arc keys bunker refuses a bad flag before it opens the identity, so the
// owner learns of the mistake at once, and not at the first request.
func TestTheBunkerRefusesBadFlags(t *testing.T) {
	home := t.TempDir()
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"keys", "bunker"}, "name the relay with --relay"},
		{[]string{"keys", "bunker", "--relay", "ftp://relay.example"}, "--relay:"},
		{[]string{"keys", "bunker", "--relay", "wss://relay.example", "--decrypt", "some"}, `--decrypt is none, self or all, not "some"`},
		{[]string{"keys", "bunker", "--relay", "wss://relay.example", "--allow-kind", "notes"}, `--allow-kind "notes" is not a kind`},
	} {
		err := arc(t, home, c.args...)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: got %v, want an error with %q", c.args, err, c.want)
		}
	}
}
