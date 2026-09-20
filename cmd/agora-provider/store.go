package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// The board lives under one directory:
//
//	posts/<id>.json   one signed post, which never changes
//	state.json        the order in which the board took the posts
//	.lock             the directory that one server holds while it runs
//
// The state names every post in board order. A post file without a record is
// recovered into the state, so a crash between the two writes loses nothing.

var temporaryPost = regexp.MustCompile(`^[a-f0-9]{64}\.json\.tmp-[A-Za-z0-9_-]+$`)

// record is one line of the state: the order, the post, and its parent.
type record struct {
	Seq    int     `json:"seq"`
	ID     string  `json:"id"`
	Parent *string `json:"parent"`
}

type stateFile struct {
	Version int      `json:"version"`
	Board   string   `json:"board"`
	Records []record `json:"records"`
}

type store struct {
	root     string
	board    string
	maxPosts int
}

func (s *store) postsDir() string          { return filepath.Join(s.root, "posts") }
func (s *store) postPath(id string) string { return filepath.Join(s.postsDir(), id+".json") }
func (s *store) statePath() string         { return filepath.Join(s.root, "state.json") }
func (s *store) lockDir() string           { return filepath.Join(s.root, ".lock") }

func (s *store) ensure() error {
	if err := os.MkdirAll(s.postsDir(), 0o700); err != nil {
		return errStorage
	}
	return nil
}

// -- the lock ---------------------------------------------------------------

// lock holds the board for one server. A lock of a process that is gone is
// taken over.
func (s *store) lock() error {
	if err := s.ensure(); err != nil {
		return err
	}
	return s.takeLock(false)
}

func (s *store) takeLock(again bool) error {
	err := os.Mkdir(s.lockDir(), 0o700)

	switch {
	case err == nil:
		pid := strconv.Itoa(os.Getpid())
		if err := os.WriteFile(filepath.Join(s.lockDir(), "pid"), []byte(pid), 0o600); err != nil {
			os.Remove(s.lockDir())
			return errLocked
		}
		return nil

	case errors.Is(err, os.ErrExist):
		if again {
			return errLocked
		}
		return s.reclaimLock()

	default:
		return errStorage
	}
}

// reclaimLock takes a lock whose process is gone.
func (s *store) reclaimLock() error {
	data, err := os.ReadFile(filepath.Join(s.lockDir(), "pid"))
	if err != nil {
		return errLocked
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 || alive(pid) {
		return errLocked
	}

	os.Remove(filepath.Join(s.lockDir(), "pid"))
	if err := os.Remove(s.lockDir()); err != nil {
		return errLocked
	}
	return s.takeLock(true)
}

func (s *store) unlock() {
	os.Remove(filepath.Join(s.lockDir(), "pid"))
	os.Remove(s.lockDir())
}

func alive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// -- reading and writing ----------------------------------------------------

// state reads the order of the board, and repairs it when a post file stands
// without a record.
func (s *store) state() (*stateFile, map[string]record, error) {
	if err := s.ensure(); err != nil {
		return nil, nil, err
	}

	held := &stateFile{Version: 1, Board: s.board, Records: []record{}}

	data, err := os.ReadFile(s.statePath())
	switch {
	case err == nil:
		decoded := &stateFile{}
		if err := json.Unmarshal(data, decoded); err != nil {
			return nil, nil, errCorrupt
		}
		if decoded.Version != 1 || decoded.Board != s.board {
			return nil, nil, errCorrupt
		}
		held = decoded
	case !errors.Is(err, os.ErrNotExist):
		return nil, nil, errCorrupt
	}

	ids, err := s.postIDs()
	if err != nil {
		return nil, nil, err
	}
	if err := checkRecords(held.Records, ids); err != nil {
		return nil, nil, err
	}

	byID := map[string]record{}
	for _, one := range held.Records {
		byID[one.ID] = one
	}

	// A post file that no record names is taken into the order, oldest id
	// first. This is the repair of a crash between the two writes.
	var orphans []string
	for _, id := range ids {
		if _, known := byID[id]; !known {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)

	for _, id := range orphans {
		held2, err := s.readPost(id)
		if err != nil {
			return nil, nil, errCorrupt
		}
		if held2.Parent != nil {
			if _, known := byID[*held2.Parent]; !known {
				return nil, nil, errCorrupt
			}
		}

		one := record{Seq: len(held.Records) + 1, ID: id, Parent: held2.Parent}
		held.Records = append(held.Records, one)
		byID[id] = one
	}

	if len(orphans) > 0 {
		if err := s.writeState(held); err != nil {
			return nil, nil, err
		}
	}
	return held, byID, nil
}

// checkRecords refuses a state that does not hold together: the order must
// run from one, an id must appear once, and a parent must stand before its
// reply.
func checkRecords(records []record, ids []string) error {
	seen := map[string]bool{}
	known := map[string]bool{}
	for _, id := range ids {
		known[id] = true
	}

	for index, one := range records {
		switch {
		case one.Seq != index+1,
			!keyPattern.MatchString(one.ID),
			seen[one.ID],
			!known[one.ID],
			one.Parent != nil && !keyPattern.MatchString(*one.Parent),
			one.Parent != nil && !seen[*one.Parent]:
			return errCorrupt
		}
		seen[one.ID] = true
	}
	return nil
}

// postIDs lists the posts on disk. Anything else in the directory, other than
// a file that a write left behind, is a broken board.
func (s *store) postIDs() ([]string, error) {
	entries, err := os.ReadDir(s.postsDir())
	if err != nil {
		return nil, errCorrupt
	}

	var ids []string
	for _, entry := range entries {
		name := entry.Name()

		switch {
		case strings.HasSuffix(name, ".json"):
			if len(name) != 69 || !keyPattern.MatchString(name[:64]) {
				return nil, errCorrupt
			}
			ids = append(ids, name[:64])
		case temporaryPost.MatchString(name):
		default:
			return nil, errCorrupt
		}
	}
	return ids, nil
}

// readPost reads one post and checks it again.
func (s *store) readPost(id string) (*post, error) {
	info, err := os.Lstat(s.postPath(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil || !info.Mode().IsRegular() {
		return nil, errCorrupt
	}

	data, err := os.ReadFile(s.postPath(id))
	if err != nil {
		return nil, errCorrupt
	}

	held, err := validatePost(data, s.board)
	if err != nil || held.ID != id {
		return nil, errCorrupt
	}
	return held, nil
}

// put takes one post. The same post again is not an error.
func (s *store) put(held *post) (map[string]any, error) {
	state, byID, err := s.state()
	if err != nil {
		return nil, err
	}

	if _, known := byID[held.ID]; known {
		existing, err := s.readPost(held.ID)
		if err != nil {
			return nil, errCorrupt
		}
		if !samePost(existing, held) {
			return nil, errConflict
		}
		return map[string]any{"post": held}, nil
	}

	if held.Parent != nil {
		if _, known := byID[*held.Parent]; !known {
			return nil, errNotFound
		}
	}
	if len(state.Records) >= s.maxPosts {
		return nil, errQuota
	}

	skew := time.Now().Unix() - held.CreatedAt
	if skew < 0 {
		skew = -skew
	}
	if skew > FreshnessSeconds {
		return nil, errStale
	}

	if err := s.writePost(held); err != nil {
		return nil, err
	}

	state.Records = append(state.Records, record{
		Seq: len(state.Records) + 1, ID: held.ID, Parent: held.Parent,
	})
	if err := s.writeState(state); err != nil {
		return nil, err
	}
	return map[string]any{"post": held}, nil
}

// read returns one post of the board.
func (s *store) read(id string) (map[string]any, error) {
	if !keyPattern.MatchString(id) {
		return nil, errInvalidRequest
	}
	if _, _, err := s.state(); err != nil {
		return nil, err
	}

	held, err := s.readPost(id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"post": held}, nil
}

// page returns the posts under one parent, in board order.
func (s *store) page(parent *string, after, limit int) ([]any, any, error) {
	state, _, err := s.state()
	if err != nil {
		return nil, nil, err
	}

	var matching []record
	for _, one := range state.Records {
		if !sameParent(one.Parent, parent) {
			continue
		}
		if after > 0 && one.Seq <= after {
			continue
		}
		matching = append(matching, one)
	}

	chosen := matching
	if len(chosen) > limit {
		chosen = chosen[:limit]
	}

	posts := make([]any, 0, len(chosen))
	for _, one := range chosen {
		held, err := s.readPost(one.ID)
		if err != nil {
			return nil, nil, errCorrupt
		}
		posts = append(posts, held)
	}

	var next any
	if len(matching) > len(chosen) && len(chosen) > 0 {
		next = chosen[len(chosen)-1].Seq
	}
	return posts, next, nil
}

// writePost writes one post through a link, so two writers cannot lose one.
func (s *store) writePost(held *post) error {
	data, err := json.Marshal(held)
	if err != nil {
		return errStorage
	}

	target := s.postPath(held.ID)
	suffix := make([]byte, 12)
	if _, err := rand.Read(suffix); err != nil {
		return errStorage
	}

	temporary := target + ".tmp-" + base64.RawURLEncoding.EncodeToString(suffix)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errStorage
	}

	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	file.Close()

	if writeErr != nil || syncErr != nil {
		os.Remove(temporary)
		return errStorage
	}

	err = os.Link(temporary, target)
	os.Remove(temporary)

	switch {
	case err == nil:
		return nil
	case errors.Is(err, os.ErrExist):
		existing, err := s.readPost(held.ID)
		if err != nil || !samePost(existing, held) {
			return errConflict
		}
		return nil
	default:
		return errStorage
	}
}

// writeState writes the order under another name and renames it, so a reader
// never sees half a file.
func (s *store) writeState(state *stateFile) error {
	state.Version = 1
	state.Board = s.board

	data, err := json.Marshal(state)
	if err != nil {
		return errStorage
	}

	temporary := s.statePath() + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return errStorage
	}
	if err := os.Rename(temporary, s.statePath()); err != nil {
		os.Remove(temporary)
		return errStorage
	}
	return nil
}

func samePost(left, right *post) bool {
	first, err := json.Marshal(left)
	if err != nil {
		return false
	}
	second, err := json.Marshal(right)
	if err != nil {
		return false
	}
	return string(first) == string(second)
}

func sameParent(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}
