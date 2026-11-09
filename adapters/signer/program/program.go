// Package program runs a program signer and sends it requests, as
// docs/devices/SPEC.md, section 3.8, defines: one JSON line for each
// request on the standard input of `<program> stdio`, and one JSON line for
// each answer on its standard output.
package program

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/gezibash/arc/core/keys"
)

// Prefix starts the key file of an identity that uses a program signer.
const Prefix = "program:"

// Find gives the path of a program signer: beside the running program
// first, then on the PATH.
func Find(name string) (string, error) {
	if name == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("%q is not the name of a program signer", name)
	}
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if info, err := os.Stat(beside); err == nil && !info.IsDir() {
			return beside, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("the program signer %s is not beside arc or on the PATH: %w", name, err)
	}
	return path, nil
}

// Client is one program signer for one identity. It starts the program at
// the first request, and keeps it for the next ones.
type Client struct {
	// Program is the path of the program signer.
	Program string
	// Dir is the folder of the identity. The program runs in it.
	Dir string

	mu     sync.Mutex
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Reader
	next   int
	broken error
	// late is closed when the read of a request that its caller left
	// ends. The next request waits for it, so answers stay in order.
	late chan struct{}
}

func (c *Client) start() error {
	cmd := exec.Command(c.Program, "stdio")
	cmd.Dir = c.Dir
	// The program asks for a passphrase on the terminal, and writes its
	// notes to the standard error of arc.
	cmd.Stderr = os.Stderr
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("the program signer did not start: %w", err)
	}
	c.cmd, c.in, c.out = cmd, in, bufio.NewReaderSize(out, 64<<10)
	return nil
}

// Call sends one request, and returns its result. When ctx ends first, the
// caller leaves; the program keeps running, and its answer is dropped.
func (c *Client) Call(ctx context.Context, method string, params ...string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.late != nil {
		select {
		case <-c.late:
			c.late = nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if c.broken != nil {
		return "", c.broken
	}
	if c.cmd == nil {
		if err := c.start(); err != nil {
			return "", err
		}
	}
	c.next++
	id := strconv.Itoa(c.next)
	line, err := json.Marshal(keys.Request{ID: id, Method: method, Params: append([]string{}, params...)})
	if err != nil {
		return "", err
	}
	type answer struct {
		response keys.Response
		err      error
	}
	done := make(chan answer, 1)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		if _, err := c.in.Write(append(line, '\n')); err != nil {
			done <- answer{err: err}
			return
		}
		text, err := c.out.ReadBytes('\n')
		if err != nil {
			done <- answer{err: err}
			return
		}
		var r keys.Response
		if err := json.Unmarshal(text, &r); err != nil {
			done <- answer{err: fmt.Errorf("the program signer gave a line that is not an answer: %w", err)}
			return
		}
		done <- answer{response: r}
	}()
	select {
	case a := <-done:
		switch {
		case a.err != nil:
			c.broken = fmt.Errorf("the program signer stopped: %w", a.err)
			c.stop()
			return "", c.broken
		case a.response.ID != id:
			c.broken = errors.New("the program signer answered another request")
			c.stop()
			return "", c.broken
		case a.response.Error != "":
			return "", errors.New(a.response.Error)
		}
		return a.response.Result, nil
	case <-ctx.Done():
		c.late = ended
		go func() {
			// A read that fails breaks the program for the next caller.
			if a := <-done; a.err != nil {
				c.mu.Lock()
				c.broken = fmt.Errorf("the program signer stopped: %w", a.err)
				c.mu.Unlock()
			}
		}()
		return "", ctx.Err()
	}
}

// stop ends the program. The caller holds mu.
func (c *Client) stop() {
	if c.cmd == nil {
		return
	}
	_ = c.in.Close()
	_ = c.cmd.Process.Kill()
	_ = c.cmd.Wait()
	c.cmd = nil
}

// Close ends the program: it reads the end of its input, and exits.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil {
		return nil
	}
	_ = c.in.Close()
	err := c.cmd.Wait()
	c.cmd = nil
	if c.broken == nil {
		c.broken = errors.New("the program signer is closed")
	}
	return err
}
