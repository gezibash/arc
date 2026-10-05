package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gezibash/arc/apps/transfer"
	"github.com/gezibash/arc/apps/transfer/client"
	"github.com/gezibash/arc/apps/transfer/direct"
	"github.com/gezibash/arc/apps/transfer/server"
)

// send gives files to one citizen in one command. It records an offer for
// each file, serves the app for that citizen, sends a message with the
// links, and waits until the citizen has each file.
func send(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("arc-transfer send", flag.ContinueOnError)
	arc := arcFlags(flags)
	state := flags.String("state", "", "the state directory (default TRANSFER_STATE, or ~/.local/state/arc-transfer)")
	text := flags.String("m", "", "the text of the message, before the links")
	wait := flags.Duration("wait", 10*time.Minute, "how long to serve the files and wait for the receiver")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() < 2 {
		return errors.New("usage: arc-transfer send [flags] <key of the receiver> <files>")
	}
	to := flags.Arg(0)
	if !direct.IsHex64(to) {
		return errors.New("the receiver is a public key of 64 hex digits")
	}
	sender, err := arc.PublicKey(ctx)
	if err != nil {
		return err
	}
	dir := server.StateDir(*state)
	var offers []direct.Offer
	lines := []string{}
	if *text != "" {
		lines = append(lines, *text)
	}
	for _, file := range flags.Args()[1:] {
		offer, err := server.Record(dir, file, []string{to})
		if err != nil {
			return err
		}
		if err := server.ForgetDelivery(dir, offer.SHA256, to); err != nil {
			return err
		}
		offers = append(offers, offer)
		lines = append(lines, direct.Link(sender, offer))
	}

	// The service must answer before the receiver reads the message.
	stop, err := serveFor(ctx, *arc, dir, to)
	if err != nil {
		return err
	}
	defer stop()
	if out, err := arc.Command(ctx, "message", "send", to, strings.Join(lines, "\n")).CombinedOutput(); err != nil {
		return fmt.Errorf("arc message send: %s", strings.TrimSpace(string(out)))
	}
	fmt.Fprintf(os.Stderr, "the message is sent; serving %d files until the receiver has them\n", len(offers))

	limit := time.After(*wait)
	for pending := offers; len(pending) > 0; {
		var still []direct.Offer
		for _, offer := range pending {
			if server.Delivered(dir, offer.SHA256, to) {
				fmt.Printf("%s\n", offer.Path)
			} else {
				still = append(still, offer)
			}
		}
		if pending = still; len(pending) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-limit:
			return fmt.Errorf("the receiver did not get %d of %d files in %s; the receiver can get a file only while a service runs: run this command again, or arc serve", len(pending), len(offers), *wait)
		case <-time.After(200 * time.Millisecond):
		}
	}
	return nil
}

// serveFor runs arc serve with the transfer app for one caller, and returns
// when the service answers. The returned function stops the service.
func serveFor(ctx context.Context, arc client.Arc, state, caller string) (stop func(), err error) {
	program, err := os.Executable()
	if err != nil {
		return nil, err
	}
	app, err := os.MkdirTemp("", "arc-transfer-send")
	if err != nil {
		return nil, err
	}
	remove := func() { _ = os.RemoveAll(app) }
	arcfile := "version = 2\n\n[serve]\ncommand = " + strconv.Quote(program) + "\nmanifest = \"./manifest.json\"\nallow = [" + strconv.Quote(caller) + "]\n"
	for name, content := range map[string][]byte{"Arcfile": []byte(arcfile), "manifest.json": transfer.Manifest} {
		if err := os.WriteFile(filepath.Join(app, name), content, 0o600); err != nil {
			remove()
			return nil, err
		}
	}
	serve := arc.Command(ctx, "serve", app)
	serve.Env = append(os.Environ(), "TRANSFER_STATE="+state)
	serve.Cancel = func() error { return serve.Process.Signal(os.Interrupt) }
	serve.WaitDelay = 5 * time.Second
	output, err := serve.StdoutPipe()
	if err != nil {
		remove()
		return nil, err
	}
	var problems strings.Builder
	serve.Stderr = &problems
	if err := serve.Start(); err != nil {
		remove()
		return nil, fmt.Errorf("arc serve: %w", err)
	}
	stop = func() {
		_ = serve.Process.Signal(os.Interrupt)
		_ = serve.Wait()
		remove()
	}
	ready := make(chan bool, 1)
	go func() {
		lines := bufio.NewScanner(output)
		for lines.Scan() {
			if strings.Contains(lines.Text(), "serves transfer") {
				ready <- true
				break
			}
		}
		_, _ = io.Copy(io.Discard, output)
		ready <- false
	}()
	select {
	case ok := <-ready:
		if ok {
			return stop, nil
		}
		stop()
		return nil, fmt.Errorf("arc serve stopped: %s", strings.TrimSpace(problems.String()))
	case <-time.After(30 * time.Second):
		stop()
		return nil, errors.New("arc serve did not start in 30 seconds")
	case <-ctx.Done():
		stop()
		return nil, ctx.Err()
	}
}
