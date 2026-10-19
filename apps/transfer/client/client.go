// Package client gets the file of a link from the transfer app of a
// sender, and gives a file to the transfer app of a receiver. It makes the
// ARC call with the arc program, so ARC signs the call with the key of this
// citizen.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/apps/transfer/files"
	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/direct"
	"github.com/pion/webrtc/v4"
)

// turn asks the first relay of a citizen for a TURN credential. arc
// signs the request with the key of the citizen.
func turn(arc arcprogram.Arc) func(context.Context) (direct.Credential, error) {
	return func(ctx context.Context) (direct.Credential, error) {
		return direct.RelayTURN(arc.Command(ctx, "relay", "turn"))
	}
}

// Get is one request for the file of a link.
type Get struct {
	Arc arcprogram.Arc
	// Link is the link of the offer.
	Link string
	// Output is the file to write. The default is the name in the link, in
	// the current directory.
	Output string
	// Options are the settings of the connection.
	Options direct.Options
	// AnswererFirst lets the sender send first in the first attempt, and
	// asks it to wait in the second.
	AnswererFirst bool
	// NoRelay stops the attempt through the TURN server of the relay after
	// the direct attempts fail.
	NoRelay bool
	// Notes gets one line for each event that a person wants to know.
	Notes io.Writer
	// State is the state directory of the transfer app of this citizen. If
	// the sender gave the file to this app with a put, Run takes it from
	// there, with no connection.
	State string
}

// Result is what Run did.
type Result struct {
	// Output is the file that Run wrote.
	Output string
	// Bytes is the number of bytes that this run took from the sender.
	Bytes int64
	// Path names the kinds of the two addresses of the connection.
	Path string
	// Elapsed is the time of the run.
	Elapsed time.Duration
	// Confirmed says that the file was here already, whole, and the run
	// only told the sender that it arrived.
	Confirmed bool
}

type getter struct {
	Get
	sender string
	offer  files.Offer
	part   *files.Part
	// unconfirmed says that the sender closed before this end said ok.
	unconfirmed atomic.Bool
}

// Run gets the file. It writes <output>.part, and renames it when the
// SHA-256 is correct. If a part file is there, it gets only the rest.
func (g Get) Run(ctx context.Context) (Result, error) {
	var result Result
	if g.Notes == nil {
		g.Notes = io.Discard
	}
	sender, offer, err := files.ParseLink(g.Link)
	if err != nil {
		return result, err
	}
	if g.Output == "" {
		g.Output = offer.Name
		if offer.Renamed && g.Notes != nil {
			fmt.Fprintln(g.Notes, "arc-transfer: the name of the file held characters that could change the terminal or hide text; arc-transfer removed them")
		}
	}
	if _, err := os.Stat(g.Output); err == nil {
		sum, err := files.HashFile(g.Output)
		if err != nil {
			return result, fmt.Errorf("%s is there already, and cannot be read: %w", g.Output, err)
		}
		if sum != offer.SHA256 {
			return result, fmt.Errorf("%s is there already", g.Output)
		}
		// The file is here and whole. The ok of an earlier get can have
		// been lost, so tell the sender again: it sends no bytes.
		fmt.Fprintf(g.Notes, "%s is there already; telling the sender that it arrived\n", g.Output)
		result.Path, err = g.confirm(ctx, sender, offer)
		result.Output, result.Confirmed = g.Output, err == nil
		return result, err
	}
	if g.State != "" {
		if taken, err := takePushed(files.ReceivedFile(g.State, sender, offer.SHA256), offer.Size, g.Output); taken || err != nil {
			result.Output, result.Path = g.Output, "put"
			return result, err
		}
	}
	part, err := files.OpenPart(g.Output, offer.SHA256, offer.Size)
	if err != nil {
		return result, err
	}
	defer part.Close()
	run := &getter{Get: g, sender: sender, offer: offer, part: part}
	resumed := part.Have()
	if resumed > 0 {
		fmt.Fprintf(g.Notes, "the part file holds %d bytes; getting the rest\n", resumed)
	}

	started := time.Now()
	err = direct.Attempts{
		Options: g.Options, AnswererFirst: g.AnswererFirst, NoRelay: g.NoRelay, TURN: turn(g.Arc), Notes: g.Notes,
		Need: func() int64 { return offer.Size - part.Have() },
	}.Run(ctx, func(ctx context.Context, options direct.Options, hold time.Duration) (err error) {
		result.Path, err = run.attempt(ctx, options, hold)
		return err
	})
	if errors.Is(err, files.ErrTooManyBytes) {
		// The part file is not a start of the file of the link.
		part.Discard()
		return result, fmt.Errorf("%w; the part file is removed", err)
	}
	if errors.Is(err, errRefused) || errors.Is(err, errNotKept) {
		return result, err
	}
	if err != nil {
		if part.Have() > resumed {
			return result, fmt.Errorf("%w; %s.part holds %d of %d bytes, run the command again to get the rest", err, g.Output, part.Have(), offer.Size)
		}
		return result, err
	}
	result.Output, result.Bytes, result.Elapsed = g.Output, part.Have()-resumed, time.Since(started)
	if run.unconfirmed.Load() {
		// The sender closed before this end could say ok, as when a slow
		// disk makes the check long: tell it once more, with no bytes.
		fmt.Fprintln(g.Notes, "the sender stopped waiting for the check; telling it that the file arrived")
		if _, err := g.confirm(ctx, sender, offer); err != nil {
			fmt.Fprintln(g.Notes, "the sender was not told; a later get of the same link tells it")
		}
	}
	return result, nil
}

// confirm tells the sender that the whole file is here: a transfer from the
// offset of the size, with no bytes.
func (g Get) confirm(ctx context.Context, sender string, offer files.Offer) (path string, err error) {
	run := &getter{Get: g, sender: sender, offer: offer}
	err = direct.Attempts{
		Options: g.Options, AnswererFirst: g.AnswererFirst, NoRelay: g.NoRelay, TURN: turn(g.Arc), Notes: g.Notes,
		Need: func() int64 { return 0 },
	}.Run(ctx, func(ctx context.Context, options direct.Options, hold time.Duration) (err error) {
		path, err = run.attempt(ctx, options, hold)
		return err
	})
	return path, err
}

// takePushed moves a file that the sender gave with a put to output. It
// says false if no such file of this size is there. The app checked the
// SHA-256 when the file arrived.
func takePushed(file string, size int64, output string) (bool, error) {
	info, err := os.Stat(file)
	if err != nil || info.Size() != size {
		return false, nil
	}
	if os.Rename(file, output) == nil {
		return true, nil
	}
	// The state directory can be on another file system than the output.
	in, err := os.Open(file)
	if err != nil {
		return true, err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(output+".part", os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return true, err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(output+".part", output)
	}
	if err != nil {
		_ = os.Remove(output + ".part")
		return true, err
	}
	return true, os.Remove(file)
}

// errRefused marks bytes that failed the check of size and SHA-256, and
// errNotKept a file that this end could not keep, such as on a full disk.
var (
	errRefused = errors.New("the bytes are refused")
	errNotKept = errors.New("the file could not be kept")
)

// offset is where the sender starts: after the bytes of the part file, or
// at the end when the file is here and only needs confirming.
func (g *getter) offset() int64 {
	if g.part == nil {
		return g.offer.Size
	}
	return g.part.Have()
}

// attempt opens one connection and takes bytes from it. It returns
// direct.ErrNoPath if the data channel did not open.
func (g *getter) attempt(ctx context.Context, options direct.Options, hold time.Duration) (path string, err error) {
	pc, err := direct.NewConnection(options)
	if err != nil {
		return "", err
	}
	defer func() { _ = pc.Close() }()
	dc, err := pc.CreateDataChannel("file", nil)
	if err != nil {
		return "", err
	}

	opened := make(chan struct{})
	closed := make(chan struct{})
	result := make(chan error, 1)
	var once sync.Once
	finish := func(err error) { once.Do(func() { result <- err }) }
	// Once end arrives, only the check of the bytes decides the get: a
	// sender that gives up waiting for ok closes the connection then.
	var ending, gone atomic.Bool
	dc.OnOpen(func() { close(opened) })
	dc.OnClose(func() { close(closed) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			if string(m.Data) != "end" {
				finish(fmt.Errorf("the sender stopped: %s", m.Data))
				return
			}
			// The sender says ok only after the size, the SHA-256 and
			// the rename pass, so that it records no delivery of bytes
			// that this end refused. The get ends before the sender reads
			// ok and closes the connection.
			ending.Store(true)
			if g.part != nil {
				beforeFinish()
				if err := g.part.Finish(); err != nil {
					switch {
					case errors.Is(err, files.ErrWrongBytes):
						finish(fmt.Errorf("%w: %w", errRefused, err))
					case errors.Is(err, files.ErrEarlyEnd):
						finish(err)
					default:
						// The bytes can be right: this end could not keep them.
						finish(fmt.Errorf("%w: %w", errNotKept, err))
					}
					_ = dc.SendText("refused: " + files.Refusal(err))
					return
				}
			}
			// A sender that gave up waiting has closed: then the ok does
			// not go, and the get tells the sender again at its end. An
			// ok that a test drops counts as sent, as a lost one does.
			if !dropOK() && (gone.Load() || dc.SendText("ok") != nil) {
				g.unconfirmed.Store(true)
			}
			afterOK()
			finish(nil)
			return
		}
		if g.part == nil {
			finish(errors.New("the sender sent bytes of a file that is here"))
			return
		}
		if err := g.part.Write(m.Data); err != nil {
			finish(err)
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			gone.Store(true)
			if !ending.Load() {
				finish(errors.New("the connection ended before the end of the file"))
			}
		}
	})

	local, err := pc.CreateOffer(nil)
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		return "", err
	}
	stun, err := direct.OfferSTUN(ctx, options, local)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(files.Fetch{Version: 1, SHA256: g.offer.SHA256, Offset: g.offset(), HoldMS: int(hold / time.Millisecond), STUN: stun, SDP: local})
	if err != nil {
		return "", err
	}
	reply, err := call(ctx, g.Arc, g.sender, string(body))
	if err != nil {
		return "", err
	}
	var remote files.Answer
	if json.Unmarshal(reply, &remote) != nil || remote.Version != 1 {
		return "", fmt.Errorf("the sender gave no answer: %.200q", reply)
	}
	if err := pc.SetRemoteDescription(remote.SDP); err != nil {
		return "", err
	}

	select {
	case <-opened:
	case <-time.After(direct.OpenWait + hold):
		return "", direct.ErrNoPath
	case <-ctx.Done():
		return "", ctx.Err()
	}
	path = direct.SelectedPath(pc)
	select {
	case err = <-result:
	case <-ctx.Done():
		err = ctx.Err()
	}
	if err != nil {
		return path, err
	}
	// The sender closes the connection when it has the "ok". If this end
	// closed first, the "ok" could stay in its buffer.
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
	}
	return path, nil
}

// call makes the ARC call to the transfer app of a citizen.
func call(ctx context.Context, arc arcprogram.Arc, key, body string) ([]byte, error) {
	command := arc.Command(ctx, "call", "transfer+arc://"+key+"/", body, "--raw", "--timeout", "30s")
	var stderr bytes.Buffer
	command.Stderr = &stderr
	reply, err := command.Output()
	if err == nil {
		return reply, nil
	}
	text := strings.TrimSpace(stderr.String())
	switch {
	case strings.Contains(text, "unknown_offer"):
		return nil, errors.New("the sender has no such offer for this citizen")
	case strings.Contains(text, "file_changed"):
		return nil, errors.New("the file of the sender changed after the offer")
	case strings.Contains(text, "busy"):
		return nil, errors.New("the app runs too many transfers now; try again later")
	case strings.Contains(text, "put_refused"):
		return nil, errors.New("the app takes no files; its owner must set TRANSFER_PUT_MAX_MIB")
	case strings.Contains(text, "too_large"):
		return nil, errors.New("the file is larger than the app takes")
	}
	return nil, fmt.Errorf("the ARC call failed: %s\nif the app of the citizen is not installed, run: arc install %s transfer", text, key)
}

// Put gives a file to the transfer app of a receiver. The app keeps the
// bytes. The receiver then takes the file with the link, with no second
// connection.
type Put struct {
	Arc arcprogram.Arc
	// To is the public key of the receiver.
	To string
	// File is the file to give.
	File string
	// Options are the settings of the connection.
	Options direct.Options
	// AnswererFirst lets the receiver send first in the first attempt, and
	// asks it to wait in the second.
	AnswererFirst bool
	// NoRelay stops the attempt through the TURN server of the relay after
	// the direct attempts fail.
	NoRelay bool
	// Notes gets one line for each event that a person wants to know.
	Notes io.Writer
}

// PutResult is what Put.Run did.
type PutResult struct {
	// Link is the link of the file, for a message to the receiver.
	Link string
	// Bytes is the number of bytes that this run gave.
	Bytes int64
	// Path names the kinds of the two addresses of the connection.
	Path    string
	Elapsed time.Duration
}

// Run gives the file. If the receiver has a start of the file, Run gives
// only the rest. If the receiver has all of it, Run gives nothing.
func (p Put) Run(ctx context.Context) (PutResult, error) {
	var result PutResult
	if p.Notes == nil {
		p.Notes = io.Discard
	}
	if !files.IsHex64(p.To) {
		return result, errors.New("the key of the receiver is not 64 hex digits")
	}
	me, err := p.Arc.PublicKey(ctx)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(p.File)
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("%s is not a regular file", p.File)
	}
	sum, err := files.HashFile(p.File)
	if err != nil {
		return result, err
	}
	offer := files.Offer{SHA256: sum, Name: filepath.Base(p.File), Size: info.Size()}
	result.Link = files.Link(me, offer)

	started := time.Now()
	err = direct.Attempts{
		Options: p.Options, AnswererFirst: p.AnswererFirst, NoRelay: p.NoRelay, TURN: turn(p.Arc), Notes: p.Notes,
		Need: func() int64 { return offer.Size },
	}.Run(ctx, func(ctx context.Context, options direct.Options, hold time.Duration) (err error) {
		result.Path, result.Bytes, err = p.attempt(ctx, options, offer, hold)
		return err
	})
	result.Elapsed = time.Since(started)
	return result, err
}

// attempt opens one connection and gives the bytes that the receiver does
// not have. It returns direct.ErrNoPath if the data channel did not open.
func (p Put) attempt(ctx context.Context, options direct.Options, offer files.Offer, hold time.Duration) (path string, sent int64, err error) {
	pc, err := direct.NewConnection(options)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = pc.Close() }()
	dc, err := pc.CreateDataChannel("file", nil)
	if err != nil {
		return "", 0, err
	}
	opened := make(chan struct{})
	answered := make(chan string, 1)
	gone := make(chan struct{})
	var goneOnce sync.Once
	dc.OnOpen(func() { close(opened) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			select {
			case answered <- string(m.Data):
			default:
			}
		}
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			goneOnce.Do(func() { close(gone) })
		}
	})

	local, err := pc.CreateOffer(nil)
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		return "", 0, err
	}
	stun, err := direct.OfferSTUN(ctx, options, local)
	if err != nil {
		return "", 0, err
	}
	body, err := json.Marshal(files.Fetch{Version: 1, Op: files.OpPut, SHA256: offer.SHA256, Size: offer.Size, HoldMS: int(hold / time.Millisecond), STUN: stun, SDP: local})
	if err != nil {
		return "", 0, err
	}
	reply, err := call(ctx, p.Arc, p.To, string(body))
	if err != nil {
		return "", 0, err
	}
	var remote files.Answer
	if json.Unmarshal(reply, &remote) != nil || remote.Version != 1 || remote.Offset < 0 || remote.Offset > offer.Size {
		return "", 0, fmt.Errorf("the receiver gave no answer: %.200q", reply)
	}
	if remote.Offset == offer.Size {
		fmt.Fprintln(p.Notes, "the receiver has the file already")
		return "", 0, nil
	}
	if remote.Offset > 0 {
		fmt.Fprintf(p.Notes, "the receiver holds %d bytes; giving the rest\n", remote.Offset)
	}
	if err := pc.SetRemoteDescription(remote.SDP); err != nil {
		return "", 0, err
	}
	select {
	case <-opened:
	case <-time.After(direct.OpenWait + hold):
		return "", 0, direct.ErrNoPath
	case <-ctx.Done():
		return "", 0, ctx.Err()
	}
	path = direct.SelectedPath(pc)
	if err := files.Send(dc, p.File, remote.Offset, offer.Size); err != nil {
		return path, 0, err
	}
	// The receiver checks the SHA-256 of all the bytes, and then says "ok".
	// An end that is gone gives no answer: the wait stops then.
	var text string
	select {
	case text = <-answered:
	case <-gone:
		select {
		case text = <-answered:
		default:
			return path, 0, errors.New("the connection to the receiver ended before it confirmed the file")
		}
	case <-time.After(putConfirmWait(offer.Size)):
		return path, 0, errors.New("the receiver did not confirm the file")
	case <-ctx.Done():
		return path, 0, ctx.Err()
	}
	if text != "ok" {
		return path, 0, fmt.Errorf("the receiver refused the file: %s", text)
	}
	return path, offer.Size - remote.Offset, nil
}
