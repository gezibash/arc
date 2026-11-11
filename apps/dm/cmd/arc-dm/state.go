package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gezibash/arc/sdk/atomicfile"
)

// readSet holds the ids of the received messages that this citizen has
// read on this device. A message id is the same on each machine, so one
// set serves each arc home that uses the state directory.
//
// The set holds ids, not a time: the time of a message is the clock of its
// sender, and a message that someone carries by hand can arrive days after
// newer messages.
type readSet map[string]bool

type readFile struct {
	Read []string `json:"read"`
}

// stateDir is the state directory: the flag, then DM_STATE, then
// ~/.local/state/arc-dm.
func stateDir(flag string) string {
	if flag != "" {
		return flag
	}
	if dir := os.Getenv("DM_STATE"); dir != "" {
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "arc-dm")
	}
	return filepath.Join(home, ".local", "state", "arc-dm")
}

func readPath(dir string) string { return filepath.Join(dir, "read.json") }

// shownPath holds the ids that arc-dm showed as parents that are not on
// this device, the last maxShown of them.
func shownPath(dir string) string { return filepath.Join(dir, "shown-missing.json") }

const maxShown = 4096

type shownFile struct {
	IDs []string `json:"ids"`
}

// loadShown reads the ids that arc-dm showed as missing. A missing or
// empty file holds none.
func loadShown(dir string) (map[string]bool, error) {
	ids, err := readShown(dir)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, id := range ids {
		set[id] = true
	}
	return set, nil
}

func readShown(dir string) ([]string, error) {
	data, err := os.ReadFile(shownPath(dir))
	if errors.Is(err, os.ErrNotExist) || (err == nil && len(data) == 0) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var file shownFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", shownPath(dir), err)
	}
	return file.IDs, nil
}

// saveShown adds ids to the ids that arc-dm showed as missing, under a
// lock, and keeps the last maxShown.
func saveShown(dir string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	unlock, err := atomicfile.Lock(shownPath(dir))
	if err != nil {
		return err
	}
	defer unlock()
	// An arc-dm killed between the write of its temporary file and the
	// rename leaves that file. Each writer holds the lock, so a temporary
	// file of this path now is such a leftover.
	if entries, err := os.ReadDir(dir); err == nil {
		temp := "." + filepath.Base(shownPath(dir)) + "-"
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), temp) {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	all, err := readShown(dir)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !slices.Contains(all, id) {
			all = append(all, id)
		}
	}
	if len(all) > maxShown {
		all = all[len(all)-maxShown:]
	}
	data, err := json.Marshal(shownFile{IDs: all})
	if err != nil {
		return err
	}
	return atomicfile.Write(shownPath(dir), data, 0o600)
}

// loadRead reads the read set. A missing file is an empty set.
func loadRead(dir string) (readSet, error) {
	data, err := os.ReadFile(readPath(dir))
	if errors.Is(err, os.ErrNotExist) {
		return readSet{}, nil
	}
	if err != nil {
		return nil, err
	}
	var file readFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("%s: %w", readPath(dir), err)
	}
	read := readSet{}
	for _, id := range file.Read {
		read[id] = true
	}
	return read, nil
}

// markRead adds the received messages of some conversations to the read
// set. It holds a lock, so two commands that mark at once lose no id.
func markRead(dir string, convs ...conversation) error {
	unlock, err := atomicfile.Lock(readPath(dir))
	if err != nil {
		return err
	}
	defer unlock()
	read, err := loadRead(dir)
	if err != nil {
		return err
	}
	changed := false
	for _, c := range convs {
		for _, e := range c.Entries {
			if !e.Mine && !read[e.ID] {
				read[e.ID] = true
				changed = true
			}
		}
	}
	if !changed {
		return nil
	}
	file := readFile{Read: make([]string, 0, len(read))}
	for id := range read {
		file.Read = append(file.Read, id)
	}
	slices.Sort(file.Read)
	data, err := json.Marshal(file)
	if err != nil {
		return err
	}
	return atomicfile.Write(readPath(dir), data, 0o600)
}
