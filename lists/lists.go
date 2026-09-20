// Package lists holds the saved sets of peers of one machine.
//
// A list lives at ~/.config/arc/lists/<tool>/<name>, one peer for each line.
// A command of that tool that names a list sends to every peer in it.
package lists

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// ErrInvalidName reports a name that a list may not take.
var ErrInvalidName = errors.New("lists: a name starts with a letter or a digit, and holds letters, digits, dot, dash and underscore")

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// Store holds the lists of one machine.
type Store struct {
	// Dir is the directory of ARC, for example ~/.config/arc.
	Dir string
}

// DefaultStore returns the store of this user.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(home, ".config", "arc")}, nil
}

func (s *Store) path(tool, name string) string {
	return filepath.Join(s.Dir, "lists", tool, name)
}

// Members returns the peers of one list, and nothing when there is no such
// list.
func (s *Store) Members(tool, name string) []string {
	if !namePattern.MatchString(tool) || !namePattern.MatchString(name) {
		return nil
	}

	data, err := os.ReadFile(s.path(tool, name))
	if err != nil {
		return nil
	}

	var members []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			members = append(members, line)
		}
	}
	return members
}

// Add puts peers in a list, and returns what the list holds now.
func (s *Store) Add(tool, name string, peers []string) ([]string, error) {
	if !namePattern.MatchString(tool) || !namePattern.MatchString(name) {
		return nil, ErrInvalidName
	}

	held := s.Members(tool, name)
	seen := map[string]bool{}
	for _, peer := range held {
		seen[peer] = true
	}

	for _, peer := range peers {
		if peer = strings.TrimSpace(peer); peer != "" && !seen[peer] {
			seen[peer] = true
			held = append(held, peer)
		}
	}
	return held, s.write(tool, name, held)
}

// Remove takes peers out of a list. With no peers, the list itself goes.
func (s *Store) Remove(tool, name string, peers []string) ([]string, error) {
	if !namePattern.MatchString(tool) || !namePattern.MatchString(name) {
		return nil, ErrInvalidName
	}

	if len(peers) == 0 {
		return nil, os.Remove(s.path(tool, name))
	}

	drop := map[string]bool{}
	for _, peer := range peers {
		drop[strings.TrimSpace(peer)] = true
	}

	var held []string
	for _, peer := range s.Members(tool, name) {
		if !drop[peer] {
			held = append(held, peer)
		}
	}
	return held, s.write(tool, name, held)
}

// Names returns the lists of one tool, in name order.
func (s *Store) Names(tool string) []string {
	if !namePattern.MatchString(tool) {
		return nil
	}

	files, err := os.ReadDir(filepath.Join(s.Dir, "lists", tool))
	if err != nil {
		return nil
	}

	var names []string
	for _, file := range files {
		if !file.IsDir() && namePattern.MatchString(file.Name()) {
			names = append(names, file.Name())
		}
	}
	sort.Strings(names)
	return names
}

func (s *Store) write(tool, name string, members []string) error {
	path := s.path(tool, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	var body strings.Builder
	for _, member := range members {
		body.WriteString(member)
		body.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(body.String()), 0o600)
}
