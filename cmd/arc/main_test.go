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
		if out, err := build.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "go build arc-signer: %v\n%s", err, out)
			return 1
		}
		if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return m.Run()
	}())
}
