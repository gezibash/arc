package direct

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/pion/webrtc/v4"
)

// HoldWait is the time that the answerer waits in the attempt in which the
// offerer sends first. It must be longer than the time that the answer
// needs to reach the offerer.
const HoldWait = time.Second

// ErrNoPath says that one attempt found no path: the data channel did not
// open. The next attempt uses the other order of the first packets, or the
// TURN server of the relay.
var ErrNoPath = errors.New("no direct path")

// relayNoPath says that the data channel did not open through the TURN
// server, after the TURN server gave an address. It names the server, so
// that the error does not read as a direct attempt (#427). It is still an
// ErrNoPath.
type relayNoPath struct{ urls []string }

func (e relayNoPath) Error() string {
	return "the data channel did not open through the TURN server " + strings.Join(e.urls, ", ")
}

func (relayNoPath) Unwrap() error { return ErrNoPath }

// NoPath is the error after the direct attempts and the relay attempt.
func NoPath(err error) error {
	return fmt.Errorf("no direct path, and no path through the relay: %w", err)
}

// Attempts is the order of the attempts of the end that makes the offer:
// a direct attempt in which the offerer sends first, a direct attempt in
// which the answerer sends first, and an attempt through the TURN server
// of the relay.
type Attempts struct {
	// Options are the settings of the direct attempts.
	Options Options
	// AnswererFirst lets the answerer send first in the first attempt, and
	// asks it to wait in the second.
	AnswererFirst bool
	// NoRelay stops after the direct attempts.
	NoRelay bool
	// OnlyRelay skips the direct attempts.
	OnlyRelay bool
	// TURN gets a TURN credential from the relay, for the last attempt.
	TURN func(context.Context) (Credential, error)
	// Need gives the number of bytes that are still to go, before the last
	// attempt.
	Need func() int64
	// Notes gets one line before each new attempt.
	Notes io.Writer
}

// Run calls attempt for each attempt, until one returns nil or an error
// that is not ErrNoPath. hold is the time that the answerer waits before
// its first packet. An attempt returns ErrNoPath if the data channel did
// not open.
func (a Attempts) Run(ctx context.Context, attempt func(ctx context.Context, options Options, hold time.Duration) error) error {
	notes := a.Notes
	if notes == nil {
		notes = io.Discard
	}
	// The order of the first packets decides on some routers. One attempt
	// makes the answerer wait, so the offerer sends first. The other lets
	// the answerer send first. With no hold, the answerer always sends
	// first: it starts before its answer leaves, and the offerer starts
	// when the answer arrives. A router of the offerer that gets a packet
	// from outside first can then block the pair, so the hold goes first.
	holds := []time.Duration{HoldWait, 0}
	if a.AnswererFirst {
		holds = []time.Duration{0, HoldWait}
	}
	if a.OnlyRelay {
		holds = nil
	}
	var err error
	for i, hold := range holds {
		err = attempt(ctx, a.Options, hold)
		if !errors.Is(err, ErrNoPath) {
			return err
		}
		if i == 0 {
			fmt.Fprintln(notes, "no direct path in the first order of the packets; trying the other order")
		}
	}
	if a.NoRelay {
		return err
	}
	if !a.OnlyRelay {
		fmt.Fprintln(notes, "no direct path; trying the TURN server of the relay, so the bytes go through the relay")
	}
	options, err := RelayOptions(ctx, a.TURN, a.Options, a.Need())
	if err != nil {
		return NoPath(err)
	}
	if err = attempt(ctx, options, 0); errors.Is(err, ErrNoPath) {
		if err == ErrNoPath && options.TURN != nil { //nolint:errorlint // Only the bare sentinel, which says nothing of the relay; a wrapped one keeps its detail.
			err = relayNoPath{urls: options.TURN.URLs}
		}
		return NoPath(err)
	}
	return err
}

// Credential is a TURN credential that a relay gave to a citizen.
type Credential struct {
	URLs     []string `json:"urls"`
	Username string   `json:"username"`
	Password string   `json:"password"`
	// QuotaLeft is the number of bytes that the citizen can still relay.
	QuotaLeft int64 `json:"quota_left"`
}

// RelayTURN runs the command "arc relay turn", and reads the credential
// that it prints. arc signs the request with the key of the citizen.
func RelayTURN(command *exec.Cmd) (Credential, error) {
	var stderr bytes.Buffer
	command.Stderr = &stderr
	out, err := command.Output()
	if err != nil {
		return Credential{}, fmt.Errorf("arc relay turn: %s", strings.TrimPrefix(strings.TrimSpace(stderr.String()), "arc: "))
	}
	var credential Credential
	if err := json.Unmarshal(out, &credential); err != nil || len(credential.URLs) == 0 {
		return Credential{}, fmt.Errorf("arc relay turn printed no credential: %.200q", out)
	}
	return credential, nil
}

// RelayOptions are the options of an attempt through the TURN server of
// the relay. need is the number of bytes that are still to go. It refuses
// before the attempt if the quota of the citizen is too small.
func RelayOptions(ctx context.Context, turn func(context.Context) (Credential, error), options Options, need int64) (Options, error) {
	if turn == nil {
		return options, errors.New("no TURN credential")
	}
	credential, err := turn(ctx)
	if err != nil {
		return options, err
	}
	// SCTP, DTLS and the acknowledgements add bytes to the payload.
	need += need / 10
	if credential.QuotaLeft < need {
		return options, fmt.Errorf("the relay lets this citizen relay %.1f MiB more today, and this file needs about %.1f MiB", float64(credential.QuotaLeft)/(1<<20), float64(need)/(1<<20))
	}
	options.TURN = &webrtc.ICEServer{URLs: credential.URLs, Username: credential.Username, Credential: credential.Password}
	return options, nil
}

// OfferSTUN is the STUN URL that an attempt through the TURN server of the
// relay gives to the other end, or empty for a direct attempt. The TURN
// server lets in only packets from the addresses in the answer. The other
// end asks the TURN server for its public address, so that address is in
// the answer, also if its own STUN server does not answer. local is the
// description of this end.
func OfferSTUN(ctx context.Context, options Options, local webrtc.SessionDescription) (string, error) {
	if options.TURN == nil {
		return "", nil
	}
	if !strings.Contains(local.SDP, "typ relay") {
		return "", NoPath(errors.New("the TURN server of the relay gave no address"))
	}
	stun, err := TURNSTUN(ctx, options.TURN)
	if err != nil {
		return "", NoPath(err)
	}
	return stun, nil
}
