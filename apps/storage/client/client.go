// Package client opens a pipe to the storage app of a storage citizen with
// the arc program. It makes the ARC call with the arc program, so ARC signs
// the call with the key of this citizen, and the manifest command "open"
// adds the pseudonym of the account. The pipe and its requests are the
// package sdk/storage.
package client

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/gezibash/arc/sdk/direct"
	"github.com/gezibash/arc/sdk/storage"
)

// Arc names the arc program and the arc home of this citizen.
type Arc struct {
	// Program is the arc program. The default is "arc" on PATH.
	Program string
	// Home is the arc home. The default is the home that arc picks.
	Home string
}

// Command makes a command of the arc program, in the arc home.
func (a Arc) Command(ctx context.Context, args ...string) *exec.Cmd {
	program := a.Program
	if program == "" {
		program = "arc"
	}
	if a.Home != "" {
		args = append([]string{"--home", a.Home}, args...)
	}
	return exec.CommandContext(ctx, program, args...)
}

// Relays asks arc for the relay URLs of this citizen. It leaves out the
// indexers.
func (a Arc) Relays(ctx context.Context) ([]string, error) {
	out, err := a.Command(ctx, "relay", "ls").Output()
	if err != nil {
		return nil, fmt.Errorf("arc relay ls: %w", err)
	}
	var relays []string
	for line := range strings.SplitSeq(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 1 && (strings.HasPrefix(fields[0], "ws://") || strings.HasPrefix(fields[0], "wss://")) {
			relays = append(relays, fields[0])
		}
	}
	return relays, nil
}

// TURN asks the first relay of this citizen for a TURN credential.
func (a Arc) TURN(ctx context.Context) (direct.Credential, error) {
	return direct.RelayTURN(a.Command(ctx, "relay", "turn"))
}

// Client opens sessions with one storage citizen through the arc program.
type Client struct {
	storage.Client
	Arc Arc
	// App is the installed name of the storage app of the storage citizen.
	// The default is "storage".
	App string
}

// Open opens a pipe. need is the number of bytes that the session sends or
// gets; the relay attempt checks it against the TURN quota.
func (c Client) Open(ctx context.Context, need int64) (*storage.Conn, error) {
	inner := c.Client
	inner.Call = c.call
	inner.TURN = c.Arc.TURN
	return inner.Open(ctx, need)
}

// call runs the manifest command "open" of the installed app. ARC signs
// the call with the account key of the citizen for this storage citizen, or
// with the device key of a machine that signs through a remote signer.
func (c Client) call(ctx context.Context, session string) ([]byte, error) {
	app := c.App
	if app == "" {
		app = "storage"
	}
	command := c.Arc.Command(ctx, app, "open", session)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	reply, err := command.Output()
	if err == nil {
		return bytes.TrimSpace(reply), nil
	}
	return nil, CallError(strings.TrimSpace(stderr.String()))
}

// CallError explains the error text of a failed ARC call to the storage app.
func CallError(text string) error {
	switch {
	case strings.Contains(text, "not_owner"):
		return errors.New("the storage citizen refused this machine: it is not an owner of an account, and it has no grant")
	case strings.Contains(text, "busy"):
		return errors.New("the storage citizen runs too many sessions now; try again later")
	}
	return fmt.Errorf("the ARC call failed: %s\nif the storage app is not installed, run: arc install <key of the storage citizen> storage", text)
}
