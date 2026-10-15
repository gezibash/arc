package direct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/webrtc/v4"
)

// Policy is the path that a citizen picks for the bulk bytes of one
// session. docs/direct/SPEC.md, section 3.7, defines each value.
type Policy string

const (
	// Auto tries the direct attempts, then the TURN server of the relay.
	Auto Policy = "auto"
	// DirectOnly tries only the direct attempts.
	DirectOnly Policy = "direct"
	// RelayOnly uses only an address on the TURN server of the relay, so the
	// other end does not see the address of this end.
	RelayOnly Policy = "turn"
	// Off opens no pipe. The bytes go in session frames over the relay.
	Off Policy = "session"
)

// ParsePolicy reads a policy. An empty value is Auto.
func ParsePolicy(value string) (Policy, error) {
	switch p := Policy(strings.TrimSpace(value)); p {
	case "":
		return Auto, nil
	case Auto, DirectOnly, RelayOnly, Off:
		return p, nil
	}
	return "", fmt.Errorf("unknown path %q: use auto, direct, turn or session", value)
}

// MaxPipes is the number of pipes that one provider process holds at a
// time. A provider answers busy to more.
const MaxPipes = 4

// The words on the data channel of a pipe. Each binary message carries
// bytes. The text "end" says that the writer sent its last byte. The text
// "ok" says that the reader read the "end".
const (
	pipeEnd = "end"
	pipeOK  = "ok"
)

// ErrPipeClosed says that the pipe closed before the end of the bytes.
var ErrPipeClosed = errors.New("the pipe closed")

// Offer is the offer of a pipe. It goes in the open request of a session.
type Offer struct {
	Version int    `json:"v"`
	SDP     string `json:"sdp"`
	// HoldMS is the time that the answerer waits before it gives the
	// addresses of the offerer to ICE.
	HoldMS int `json:"hold_ms,omitempty"`
	// STUN is the STUN URL of the TURN server of the offerer, in a relay
	// attempt.
	STUN string `json:"stun,omitempty"`
}

// Check says if the offer is one that an answerer takes.
func (o Offer) Check() error {
	if o.Version != 1 || o.SDP == "" || o.HoldMS < 0 ||
		time.Duration(o.HoldMS)*time.Millisecond > MaxHold || (o.STUN != "" && !IsIPSTUN(o.STUN)) {
		return errors.New("the offer of the pipe is not valid")
	}
	return nil
}

// Pipe is one reliable, ordered byte stream in each direction, for one
// session. It is a WebRTC data channel.
type Pipe struct {
	pc   *webrtc.PeerConnection
	hold time.Duration

	dc     atomic.Pointer[webrtc.DataChannel]
	opened chan struct{}
	dead   chan struct{}
	acked  chan struct{}
	low    chan struct{}
	r      *io.PipeReader
	w      *io.PipeWriter

	openOnce, deadOnce, ackOnce sync.Once
}

func newPipe(pc *webrtc.PeerConnection, hold time.Duration) *Pipe {
	r, w := io.Pipe()
	p := &Pipe{pc: pc, hold: hold, opened: make(chan struct{}), dead: make(chan struct{}),
		acked: make(chan struct{}), low: make(chan struct{}, 1), r: r, w: w}
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			p.die()
		}
	})
	return p
}

// attach takes the data channel of the pipe. A second channel is closed.
func (p *Pipe) attach(dc *webrtc.DataChannel) {
	if !p.dc.CompareAndSwap(nil, dc) {
		_ = dc.Close()
		return
	}
	dc.SetBufferedAmountLowThreshold(BufferLow)
	dc.OnBufferedAmountLow(func() {
		select {
		case p.low <- struct{}{}:
		default:
		}
	})
	dc.OnOpen(func() { p.openOnce.Do(func() { close(p.opened) }) })
	dc.OnClose(p.die)
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if !m.IsString {
			// The write blocks until Read takes the bytes. SCTP then
			// stops the writer when the buffer of this end is full.
			_, _ = p.w.Write(m.Data)
			return
		}
		switch text := string(m.Data); text {
		case pipeEnd:
			_ = p.w.Close()
			_ = dc.SendText(pipeOK)
		case pipeOK:
			p.ackOnce.Do(func() { close(p.acked) })
		default:
			_ = p.w.CloseWithError(fmt.Errorf("the other end stopped: %.200s", text))
		}
	})
}

func (p *Pipe) die() {
	p.deadOnce.Do(func() {
		close(p.dead)
		// An "end" that came before keeps its io.EOF.
		_ = p.w.CloseWithError(ErrPipeClosed)
	})
}

// Wait waits until the data channel opens. It returns ErrNoPath if the
// channel does not open in OpenWait plus the hold, or if the connection
// fails before the SCTP association connects, and then closes the pipe.
func (p *Pipe) Wait(ctx context.Context) error {
	timer := time.NewTimer(OpenWait + p.hold)
	defer timer.Stop()
	select {
	case <-p.opened:
		return nil
	case <-p.dead:
		// pion calls the open handler in a goroutine of its own, and this
		// end can read the close before the acknowledgement of the open. So
		// a channel that the other end opens, writes and closes at once can
		// die with no open. The channel has an ID only after the SCTP
		// association connected: that attempt found a path, and the other
		// end can have started. The next attempt would be a replay, so the
		// dead pipe goes to the caller, whose read or write fails.
		if dc := p.dc.Load(); dc != nil && dc.ID() != nil {
			return nil
		}
		_ = p.Close()
		return ErrNoPath
	case <-timer.C:
		_ = p.Close()
		return ErrNoPath
	case <-ctx.Done():
		_ = p.Close()
		return ctx.Err()
	}
}

// Read reads the bytes that the other end wrote. It returns io.EOF after
// the other end called CloseWrite, and ErrPipeClosed if the pipe closed
// before.
func (p *Pipe) Read(b []byte) (int, error) { return p.r.Read(b) }

// Write writes b in messages of at most ChunkSize. It waits while the
// buffer of the data channel holds more than BufferHigh bytes, until it
// holds BufferLow bytes or less.
func (p *Pipe) Write(b []byte) (int, error) {
	dc := p.dc.Load()
	if dc == nil {
		return 0, ErrPipeClosed
	}
	written := 0
	for written < len(b) {
		n := min(len(b)-written, ChunkSize)
		if err := dc.Send(b[written : written+n]); err != nil {
			return written, ErrPipeClosed
		}
		written += n
		if dc.BufferedAmount() > BufferHigh {
			select {
			case <-p.low:
			case <-p.dead:
				return written, ErrPipeClosed
			}
		}
	}
	return written, nil
}

// CloseWrite says that this end sent its last byte. It waits until the
// other end read all bytes.
func (p *Pipe) CloseWrite(ctx context.Context) error {
	dc := p.dc.Load()
	if dc == nil || dc.SendText(pipeEnd) != nil {
		return ErrPipeClosed
	}
	select {
	case <-p.acked:
		return nil
	case <-p.dead:
		return ErrPipeClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close closes the pipe. A blocked Read returns ErrPipeClosed.
func (p *Pipe) Close() error {
	_ = p.r.CloseWithError(ErrPipeClosed)
	err := p.pc.Close()
	p.die()
	return err
}

// Path names the path of the pipe, such as "direct (srflx to srflx)" or
// "turn (relay)".
func (p *Pipe) Path() string {
	path := SelectedPath(p.pc)
	if path == "relay" {
		return "turn (relay)"
	}
	return "direct (" + path + ")"
}

// Report is the line that a command writes after the bytes of a session:
// the path, the number of bytes, the time and the speed.
func Report(path string, n int64, elapsed time.Duration) string {
	seconds := max(elapsed.Seconds(), 1e-3)
	return fmt.Sprintf("path %s: %d bytes in %.1f s, %.1f Mbit/s", path, n, elapsed.Seconds(), float64(n)*8/seconds/1e6)
}

// SessionPath is the name of the path of bytes in session frames.
const SessionPath = "session (relay frames)"

// DialConfig holds the settings of the end that makes the offer.
type DialConfig struct {
	// Policy is Auto, DirectOnly or RelayOnly.
	Policy  Policy
	Options Options
	// AnswererFirst lets the answerer send first in the first attempt, and
	// asks it to wait in the second.
	AnswererFirst bool
	// TURN gets a TURN credential, for a relay attempt.
	TURN func(context.Context) (Credential, error)
	// Need is the number of bytes to go, for the quota check before a relay
	// attempt. Zero checks only that some quota is left.
	Need int64
	// Notes gets one line before each new attempt.
	Notes io.Writer
}

// Dial opens a pipe with the attempts of the policy. For each attempt, it
// calls open with an offer. open starts a new session that carries the
// offer, and returns the SDP of the answer. Dial returns the first pipe
// whose data channel opens. An error of open that is not ErrNoPath stops
// the attempts.
func Dial(ctx context.Context, c DialConfig, open func(context.Context, Offer) (string, error)) (*Pipe, error) {
	if c.Policy == Off {
		return nil, errors.New("the policy opens no pipe")
	}
	var pipe *Pipe
	err := Attempts{
		Options: c.Options, AnswererFirst: c.AnswererFirst, TURN: c.TURN, Notes: c.Notes,
		NoRelay: c.Policy == DirectOnly, OnlyRelay: c.Policy == RelayOnly,
		Need: func() int64 { return max(c.Need, 1) },
	}.Run(ctx, func(ctx context.Context, options Options, hold time.Duration) error {
		p, err := dialOnce(ctx, options, hold, open)
		pipe = p
		return err
	})
	if err != nil {
		return nil, err
	}
	return pipe, nil
}

func dialOnce(ctx context.Context, options Options, hold time.Duration, open func(context.Context, Offer) (string, error)) (*Pipe, error) {
	pc, err := NewConnection(options)
	if err != nil {
		return nil, err
	}
	p := newPipe(pc, hold)
	dc, err := pc.CreateDataChannel("pipe", nil)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	p.attach(dc)
	local, err := pc.CreateOffer(nil)
	if err == nil {
		local, err = Describe(pc, local)
	}
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	stun, err := OfferSTUN(ctx, options, local)
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	answer, err := open(ctx, Offer{Version: 1, SDP: local.SDP, HoldMS: int(hold / time.Millisecond), STUN: stun})
	if err == nil {
		err = pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer})
	}
	if err != nil {
		_ = p.Close()
		return nil, err
	}
	if err := p.Wait(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// AcceptConfig holds the settings of the end that answers.
type AcceptConfig struct {
	// Policy is Auto, DirectOnly or RelayOnly. Auto and DirectOnly answer
	// with all addresses of this end.
	Policy  Policy
	Options Options
	// TURN gets a TURN credential, for the policy RelayOnly.
	TURN func(context.Context) (Credential, error)
}

// Accept answers an offer. It returns the pipe and the SDP of the answer.
// The caller sends the answer, then calls Wait.
func Accept(ctx context.Context, c AcceptConfig, o Offer) (*Pipe, string, error) {
	if err := o.Check(); err != nil {
		return nil, "", err
	}
	options := c.Options
	options.PeerSTUN = o.STUN
	switch c.Policy {
	case Off:
		return nil, "", errors.New("the policy opens no pipe")
	case RelayOnly:
		if c.TURN == nil {
			return nil, "", errors.New("no TURN credential")
		}
		var err error
		if options, err = RelayOptions(ctx, c.TURN, options, 1); err != nil {
			return nil, "", err
		}
	}
	pc, err := NewConnection(options)
	if err != nil {
		return nil, "", err
	}
	hold := time.Duration(o.HoldMS) * time.Millisecond
	p := newPipe(pc, hold)
	pc.OnDataChannel(p.attach)
	remote := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: o.SDP}
	var held []webrtc.ICECandidateInit
	if hold > 0 {
		held = HoldCandidates(&remote)
	}
	if err := pc.SetRemoteDescription(remote); err != nil {
		_ = p.Close()
		return nil, "", err
	}
	if hold > 0 {
		time.AfterFunc(hold, func() {
			for _, candidate := range held {
				_ = pc.AddICECandidate(candidate)
			}
		})
	}
	local, err := pc.CreateAnswer(nil)
	if err == nil {
		local, err = Describe(pc, local)
	}
	if err == nil && options.TURN != nil && !strings.Contains(local.SDP, "typ relay") {
		err = errors.New("the TURN server of the relay gave no address")
	}
	if err != nil {
		_ = p.Close()
		return nil, "", err
	}
	return p, local.SDP, nil
}

// EnvOptions are the options of an end from the environment:
//
//	ARC_STUN      the STUN server, or "none"; unset asks the first relay on
//	              port 3478
//	ARC_LOOPBACK  if set, use the loopback address too
func EnvOptions(relays []string) Options {
	return Options{STUN: STUNURL(os.Getenv("ARC_STUN"), relays), Loopback: os.Getenv("ARC_LOOPBACK") != ""}
}

// ProviderConfig is the config of a provider program from the environment
// that arc serve gives it: ARC_PATH, ARC_RELAYS, and the variables of
// EnvOptions. With ARC_PATH=turn, the provider runs "arc relay turn" for a
// credential, with the program ARC_PROGRAM, the home ARC_HOME and the key
// ARC_IDENTITY.
func ProviderConfig() (AcceptConfig, error) {
	policy, err := ParsePolicy(os.Getenv("ARC_PATH"))
	if err != nil {
		return AcceptConfig{}, fmt.Errorf("ARC_PATH: %w", err)
	}
	c := AcceptConfig{Policy: policy, Options: EnvOptions(strings.Fields(os.Getenv("ARC_RELAYS")))}
	if policy == RelayOnly {
		c.TURN = func(ctx context.Context) (Credential, error) {
			program := os.Getenv("ARC_PROGRAM")
			if program == "" {
				program = "arc"
			}
			var args []string
			if home := os.Getenv("ARC_HOME"); home != "" {
				args = append(args, "--home", home)
			}
			if key := os.Getenv("ARC_IDENTITY"); key != "" {
				args = append(args, "--key", key)
			}
			return RelayTURN(exec.CommandContext(ctx, program, append(args, "relay", "turn")...))
		}
	}
	return c, nil
}
