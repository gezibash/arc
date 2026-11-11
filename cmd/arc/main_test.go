package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestMain builds arc-signer, the program signer of each identity that the
// tests make, and puts it on the PATH, where arc finds it.
func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "arc-signer-")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		build := exec.Command("go", "build", "-o", filepath.Join(dir, "arc-signer"), "../arc-signer")
		// The signer is built without -race: each command starts one, and a
		// race build of it made this package pass the 10 minutes of CI.
		// adapters/signer/program runs arc-signer with -race.
		build.Env = append(os.Environ(), "GOFLAGS=")
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build arc-signer: %v\n%s", err, out)
			return 1
		}
		if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		// The two ends of a pipe on this machine find each other on the
		// loopback address, with no STUN server: the test relay serves no
		// STUN. It is set once for each test, so that a test that opens a
		// pipe can run in parallel with the others.
		for name, value := range map[string]string{"ARC_LOOPBACK": "1", "ARC_STUN": "none"} {
			if err := os.Setenv(name, value); err != nil {
				fmt.Fprintln(os.Stderr, err)
				return 1
			}
		}
		return m.Run()
	}())
}
