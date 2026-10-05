// Package client gets the file of a link from the transfer app of a
// sender. It makes the ARC call with the arc program, so ARC signs the call
// with the key of this citizen.
package client

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gezibash/arc/apps/transfer/direct"
	"github.com/pion/webrtc/v4"
)

// holdWait is the time that the receiver asks the sender to wait in the
// attempt in which the receiver sends first.
const holdWait = time.Second

// errNoPath says that one attempt found no direct path. The next attempt
// uses the other order of the first packets.
var errNoPath = errors.New("no direct path")

// errTooManyBytes says that the sender wrote more bytes than the size in the
// link. The receiver does not write these bytes.
var errTooManyBytes = errors.New("the sender wrote more bytes than the link says")

// Arc names the arc program and the arc home of this citizen.
type Arc struct {
	// Program is the arc program. The default is "arc" on PATH.
	Program string
	// Home is the arc home. The default is the home that arc picks.
	Home string
}

func (a Arc) command(ctx context.Context, args ...string) *exec.Cmd {
	program := a.Program
	if program == "" {
		program = "arc"
	}
	if a.Home != "" {
		args = append([]string{"--home", a.Home}, args...)
	}
	return exec.CommandContext(ctx, program, args...)
}

// PublicKey asks arc for the public key of this citizen.
func (a Arc) PublicKey(ctx context.Context) (string, error) {
	out, err := a.command(ctx, "whoami").Output()
	if err != nil {
		return "", fmt.Errorf("arc whoami: %w", err)
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if key := strings.TrimSpace(line); direct.IsHex64(key) {
			return key, nil
		}
	}
	return "", errors.New("arc whoami printed no public key")
}

// Get is one request for the file of a link.
type Get struct {
	Arc Arc
	// Link is the link of the offer.
	Link string
	// Output is the file to write. The default is the name in the link, in
	// the current directory.
	Output string
	// Options are the settings of the connection.
	Options direct.Options
	// HoldFirst asks the sender to wait in the first attempt, not in the
	// second.
	HoldFirst bool
	// Notes gets one line for each event that a person wants to know.
	Notes io.Writer
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
}

type getter struct {
	Get
	sender string
	offer  direct.Offer
	file   *os.File
	sum    hash.Hash
	// have is the number of bytes of the file that the part file holds.
	have atomic.Int64
}

// Run gets the file. It writes <output>.part, and renames it when the
// SHA-256 is correct. If a part file is there, it gets only the rest.
func (g Get) Run(ctx context.Context) (Result, error) {
	var result Result
	if g.Notes == nil {
		g.Notes = io.Discard
	}
	sender, offer, err := direct.ParseLink(g.Link)
	if err != nil {
		return result, err
	}
	if g.Output == "" {
		g.Output = offer.Name
	}
	if _, err := os.Stat(g.Output); err == nil {
		return result, fmt.Errorf("%s is there already", g.Output)
	}
	run := &getter{Get: g, sender: sender, offer: offer, sum: sha256.New()}
	part := g.Output + ".part"
	if err := run.openPart(part); err != nil {
		return result, err
	}
	defer func() { _ = run.file.Close() }()
	resumed := run.have.Load()
	if resumed > 0 {
		fmt.Fprintf(g.Notes, "the part file holds %d bytes; getting the rest\n", resumed)
	}

	// The order of the first packets decides on some routers. One attempt
	// lets the sender send first. The other makes it wait.
	holds := []time.Duration{0, holdWait}
	if g.HoldFirst {
		holds = []time.Duration{holdWait, 0}
	}
	started := time.Now()
	for i, hold := range holds {
		result.Path, err = run.attempt(ctx, hold)
		if !errors.Is(err, errNoPath) {
			break
		}
		if i == 0 {
			fmt.Fprintln(g.Notes, "no direct path in the first order of the packets; trying the other order")
		}
	}
	if errors.Is(err, errTooManyBytes) {
		// The part file is not a start of the file of the link.
		_ = run.file.Close()
		_ = os.Remove(part)
		return result, fmt.Errorf("%w; the part file is removed", err)
	}
	if err != nil {
		if run.have.Load() > resumed {
			return result, fmt.Errorf("%w; %s holds %d of %d bytes, run the command again to get the rest", err, part, run.have.Load(), offer.Size)
		}
		return result, err
	}

	if err := run.file.Close(); err != nil {
		return result, err
	}
	if got := hex.EncodeToString(run.sum.Sum(nil)); run.have.Load() != offer.Size || got != offer.SHA256 {
		_ = os.Remove(part)
		return result, fmt.Errorf("the bytes do not have the SHA-256 of the link (%d bytes, %s); the part file is removed", run.have.Load(), got)
	}
	if err := os.Rename(part, g.Output); err != nil {
		return result, err
	}
	result.Output, result.Bytes, result.Elapsed = g.Output, run.have.Load()-resumed, time.Since(started)
	return result, nil
}

// openPart opens the part file to add bytes, and reads the bytes that it
// holds into the SHA-256.
func (g *getter) openPart(part string) error {
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err == nil && info.Size() > g.offer.Size {
		// The part file is not a start of this file.
		err = f.Truncate(0)
	}
	var n int64
	if err == nil {
		n, err = io.Copy(g.sum, f)
	}
	if err != nil {
		_ = f.Close()
		return err
	}
	g.file = f
	g.have.Store(n)
	return nil
}

// attempt opens one connection and takes bytes from it. It returns
// errNoPath if the data channel did not open.
func (g *getter) attempt(ctx context.Context, hold time.Duration) (path string, err error) {
	pc, err := direct.NewConnection(g.Options)
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
	dc.OnOpen(func() { close(opened) })
	dc.OnClose(func() { close(closed) })
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		if m.IsString {
			if string(m.Data) != "end" {
				finish(fmt.Errorf("the sender stopped: %s", m.Data))
				return
			}
			_ = dc.SendText("ok")
			finish(nil)
			return
		}
		// The size in the link is the limit. A sender that writes more must
		// not fill the disk.
		if g.have.Load()+int64(len(m.Data)) > g.offer.Size {
			finish(errTooManyBytes)
			return
		}
		if _, err := g.file.Write(m.Data); err != nil {
			finish(err)
			return
		}
		g.sum.Write(m.Data)
		g.have.Add(int64(len(m.Data)))
	})
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			finish(errors.New("the connection ended before the end of the file"))
		}
	})

	local, err := pc.CreateOffer(nil)
	if err == nil {
		local, err = direct.Describe(pc, local)
	}
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(direct.Fetch{Version: 1, SHA256: g.offer.SHA256, Offset: g.have.Load(), HoldMS: int(hold / time.Millisecond), SDP: local})
	if err != nil {
		return "", err
	}
	reply, err := g.call(ctx, string(body))
	if err != nil {
		return "", err
	}
	var remote direct.Answer
	if json.Unmarshal(reply, &remote) != nil || remote.Version != 1 {
		return "", fmt.Errorf("the sender gave no answer: %.200q", reply)
	}
	if err := pc.SetRemoteDescription(remote.SDP); err != nil {
		return "", err
	}

	select {
	case <-opened:
	case <-time.After(direct.OpenWait + hold):
		return "", errNoPath
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

// call makes the ARC call to the app of the sender.
func (g *getter) call(ctx context.Context, body string) ([]byte, error) {
	command := g.Arc.command(ctx, "call", "transfer+arc://"+g.sender+"/", body, "--raw", "--timeout", "30s")
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
		return nil, errors.New("the sender runs too many transfers now; try again later")
	}
	return nil, fmt.Errorf("the ARC call failed: %s\nif the app of the sender is not installed, run: arc install %s transfer", text, g.sender)
}
