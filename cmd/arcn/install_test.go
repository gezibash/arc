package main

import (
	"net"
	"strings"
	"testing"

	"github.com/gezibash/arc/delivery/keys"
)

func arcn(t *testing.T, home string, args ...string) error {
	t.Helper()
	command := root()
	command.SetArgs(append([]string{"--home", home}, args...))
	return command.Execute()
}

// When the relay is down, install says so. It does not say that the
// provider announced nothing.
func TestInstallSaysWhenNoRelayAnswered(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dead := "ws://" + l.Addr().String()
	l.Close()

	home := t.TempDir()
	if err := arcn(t, home, "key", "new"); err != nil {
		t.Fatal(err)
	}
	if err := arcn(t, home, "relay", "add", dead); err != nil {
		t.Fatal(err)
	}
	provider := keys.Generate().Public.Hex()
	err = arcn(t, home, "install", provider, "journal", "--yes")
	if err == nil || !strings.Contains(err.Error(), "no relay answered") || !strings.Contains(err.Error(), dead) {
		t.Fatalf("install returned %v, want an error that names the relay %s", err, dead)
	}
}
