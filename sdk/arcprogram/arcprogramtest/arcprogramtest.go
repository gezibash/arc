// Package arcprogramtest runs a relay on this machine for the tests of a
// client program. The tests then run the arc program and the client
// program as a person does.
package arcprogramtest

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
)

// Net is a relay on this machine, and the arc program that serves it.
type Net struct {
	// Arc is the path of the arc program.
	Arc string
	// Relay is the URL of the relay.
	Relay string
	dir   string
	relay *exec.Cmd
}

// Start builds the arc program into dir, and starts a relay on a free port
// of this machine. Close stops the relay.
func Start(dir string) (*Net, error) {
	n := &Net{Arc: filepath.Join(dir, "arc"), dir: dir}
	if out, err := exec.Command("go", "build", "-o", n.Arc, "github.com/gezibash/arc/cmd/arc").CombinedOutput(); err != nil {
		return nil, fmt.Errorf("go build arc: %v\n%s", err, out)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	_ = listener.Close()
	n.relay = exec.Command(n.Arc, "--home", filepath.Join(dir, "relay"), "relay", "serve", "--listen", address, "--stun", "none")
	if err := n.relay.Start(); err != nil {
		return nil, fmt.Errorf("arc relay serve: %w", err)
	}
	if !WaitUntil(10*time.Second, func() bool {
		conn, err := net.Dial("tcp", address)
		if err == nil {
			_ = conn.Close()
		}
		return err == nil
	}) {
		n.Close()
		return nil, fmt.Errorf("the relay did not start on %s", address)
	}
	n.Relay = "ws://" + address
	return n, nil
}

// Close stops the relay.
func (n *Net) Close() {
	_ = n.relay.Process.Kill()
	_ = n.relay.Wait()
}

// Citizen makes a citizen with a new key, in a new arc home under the
// directory of the net, and adds the relay. It returns the citizen and its
// public key.
func (n *Net) Citizen(name string) (arcprogram.Arc, string, error) {
	home, err := os.MkdirTemp(n.dir, name)
	if err != nil {
		return arcprogram.Arc{}, "", err
	}
	arc := arcprogram.Arc{Program: n.Arc, Home: home}
	for _, args := range [][]string{{"keys", "gen"}, {"relay", "add", n.Relay}} {
		if out, err := arc.Command(context.Background(), args...).CombinedOutput(); err != nil {
			return arcprogram.Arc{}, "", fmt.Errorf("arc %v: %v\n%s", args, err, out)
		}
	}
	key, err := arc.PublicKey(context.Background())
	return arc, key, err
}

// WaitUntil calls done each 100 ms until it returns true, or until the
// limit. It says whether done returned true.
func WaitUntil(limit time.Duration, done func() bool) bool {
	for end := time.Now().Add(limit); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if done() {
			return true
		}
	}
	return done()
}
