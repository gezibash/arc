package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gezibash/arc/apps/transfer/direct"
)

// StateDir is the directory that holds the offers of a sender. The command
// that records an offer and the service use the same directory: the given
// one, or TRANSFER_STATE, or ~/.local/state/arc-transfer.
func StateDir(given string) string {
	if given != "" {
		return given
	}
	if dir := os.Getenv("TRANSFER_STATE"); dir != "" {
		return dir
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "arc-transfer")
}

func offerFile(state, sum string) string {
	return filepath.Join(state, "offers", sum+".json")
}

// Record makes the offer of a file, and writes it to the state directory.
// The file of an offer is offers/<sha256>.json.
func Record(state, file string, to []string) (direct.Offer, error) {
	var o direct.Offer
	path, err := filepath.Abs(file)
	if err != nil {
		return o, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return o, err
	}
	if !info.Mode().IsRegular() {
		return o, fmt.Errorf("%s is not a regular file", path)
	}
	sum, err := hashFile(path)
	if err != nil {
		return o, err
	}
	o = direct.Offer{SHA256: sum, Path: path, Name: filepath.Base(path), Size: info.Size(), Modified: info.ModTime(), To: to}

	if err := os.MkdirAll(filepath.Join(state, "offers"), 0o700); err != nil {
		return o, err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return o, err
	}
	// The service reads the file while this command writes it, so the new
	// file replaces the old file in one step.
	next := offerFile(state, sum) + ".next"
	if err := os.WriteFile(next, append(raw, '\n'), 0o600); err != nil {
		return o, err
	}
	return o, os.Rename(next, offerFile(state, sum))
}

// load returns the offer with this SHA-256. The caller checks first that
// sum is 64 hex digits, so sum cannot name a file out of the directory.
func load(state, sum string) (direct.Offer, error) {
	var o direct.Offer
	raw, err := os.ReadFile(offerFile(state, sum))
	if err != nil {
		return o, err
	}
	return o, json.Unmarshal(raw, &o)
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}
