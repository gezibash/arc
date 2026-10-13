package citizen

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"fiatjaf.com/nostr"
)

// SignerOnly removes the relay errors around a signer error, keeps the text
// that says what the command did, and keeps the error chain.
func TestSignerOnlyKeepsWhatTheCommandDid(t *testing.T) {
	citizen := nostr.Generate().Public()
	signer := &SignerError{Citizen: citizen, Err: context.DeadlineExceeded}
	says := "this needs the signer of the citizen " + citizen.Hex() + ", and the signer did not answer; start arc keys bunker on the machine that holds the seed"
	auth := fmt.Errorf("relay ws://127.0.0.1:7447: error signing auth event: %w", signer)

	cases := []struct {
		name string
		err  error
		want string
	}{
		{"through a relay", fmt.Errorf("this machine holds no storage list, and no relay answered: %w", auth), "this machine holds no storage list, and " + says},
		{"only a relay", fmt.Errorf("no relay answered: %w", auth), says},
		{"no relay", fmt.Errorf("this machine holds no data root of generation 2, and %w", signer), "this machine holds no data root of generation 2, and " + says},
		{"no signer", errors.New("relay ws://127.0.0.1:7447: refused"), "relay ws://127.0.0.1:7447: refused"},
	}
	for _, c := range cases {
		got := SignerOnly(c.err)
		if got.Error() != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
		if c.name != "no signer" && !errors.Is(got, context.DeadlineExceeded) {
			t.Errorf("%s: the error chain is lost", c.name)
		}
	}
}
