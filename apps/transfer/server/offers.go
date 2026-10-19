package server

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gezibash/arc/apps/transfer/files"
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
func Record(state, file string, to []string) (files.Offer, error) {
	var o files.Offer
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
	sum, err := files.HashFile(path)
	if err != nil {
		return o, err
	}
	o = files.Offer{SHA256: sum, Path: path, Name: filepath.Base(path), Size: info.Size(), Modified: info.ModTime(), To: to}

	if err := os.MkdirAll(filepath.Join(state, "offers"), 0o700); err != nil {
		return o, err
	}
	raw, err := json.Marshal(o)
	if err != nil {
		return o, err
	}
	// The service reads the file while this command writes it, so the new
	// file replaces the old file in one step, and is on the disk first.
	return o, files.WriteFile(offerFile(state, sum), append(raw, '\n'), 0o600)
}

// load returns the offer with this SHA-256. The caller checks first that
// sum is 64 hex digits, so sum cannot name a file out of the directory.
func load(state, sum string) (files.Offer, error) {
	var o files.Offer
	raw, err := os.ReadFile(offerFile(state, sum))
	if err != nil {
		return o, err
	}
	return o, json.Unmarshal(raw, &o)
}

func deliveryFile(state, sum, to string) string {
	return filepath.Join(state, "delivered", sum+"-"+to)
}

// recordDelivery writes that a citizen has all the bytes of an offer. The
// file delivered/<sha256>-<key> holds the time and the path of the
// connection.
func recordDelivery(state, sum, to, path string) error {
	if err := os.MkdirAll(filepath.Join(state, "delivered"), 0o700); err != nil {
		return err
	}
	return files.WriteFile(deliveryFile(state, sum, to), []byte(time.Now().UTC().Format(time.RFC3339)+" "+path+"\n"), 0o600)
}

// Delivered says whether a citizen got all the bytes of an offer, and on
// which path. The service records a delivery when the receiver confirms
// the last byte.
func Delivered(state, sum, to string) (path string, ok bool) {
	data, err := os.ReadFile(deliveryFile(state, sum, to))
	if err != nil {
		return "", false
	}
	_, path, _ = strings.Cut(strings.TrimSpace(string(data)), " ")
	return path, true
}

// ForgetDelivery removes the record of a delivery. A sender that gives the
// same file to the same citizen again then waits for the new delivery.
func ForgetDelivery(state, sum, to string) error {
	err := os.Remove(deliveryFile(state, sum, to))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
