package relay

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/gezibash/arc/core/transport"
)

// ErrNoAnswer is the cause of the timeout of a call of the relay: the
// relay, not the caller, ended the call.
var ErrNoAnswer = errors.New("the relay gave no answer in time")

// refused says whether an error is the answer of a relay that did not take
// an event.
func refused(err error) bool { return strings.HasPrefix(err.Error(), "msg: ") }

// upgradeStatus finds the HTTP status of a relay that refused the WebSocket.
var upgradeStatus = regexp.MustCompile(`expected handshake response status code 101 but got (\d{3})`)

// Refusal is the reason of a relay that refused a call: the reason of a
// CLOSED or of an OK that is false, a request for authentication, or an
// HTTP status other than 101 for the WebSocket. ok is false for each other
// error, such as a relay that gave no answer (#463). The relay chooses
// the reason: a caller escapes it before it prints it.
func Refusal(err error) (reason string, ok bool) {
	if err == nil {
		return "", false
	}
	var answer answerError
	switch {
	case errors.As(err, &answer):
		return answer.reason, true
	case errors.Is(err, errAuthRequired):
		return "it asks for authentication", true
	}
	if m := upgradeStatus.FindStringSubmatch(err.Error()); m != nil {
		return "HTTP " + m[1] + " for the WebSocket", true
	}
	for e := err; e != nil; e = errors.Unwrap(e) {
		if text, ok := strings.CutPrefix(e.Error(), "msg: "); ok {
			return text, true
		}
	}
	return "", false
}

// answerError is the reason of a CLOSED message: an answer of the relay.
type answerError struct{ reason string }

func (e answerError) Error() string { return "the relay closed the query: " + e.reason }

// answered says whether an error is an answer of the relay, so the connection
// that carried it works.
func answered(err error) bool {
	var answer answerError
	return refused(err) || errors.Is(err, errAuthRequired) || errors.As(err, &answer)
}

// named adds the relay to an error.
func (r Relay) named(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("relay %s: %w", r.URL, err)
}

// unheard marks the answer of a relay that no one got a live event, which
// khatru gives when no watch matched it: the event reached no one.
func unheard(err error) error {
	if err != nil && strings.Contains(err.Error(), "mute: no one was listening") {
		return transport.NoListener(err)
	}
	return err
}

// closed is the error for the reason of a CLOSED message.
func closed(reason string) error {
	if strings.HasPrefix(reason, "auth-required") {
		return errAuthRequired
	}
	return answerError{reason}
}
