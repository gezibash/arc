package main

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The files of one owner live in objects/<owner>/<id>.json. An id never
// repeats, because it holds the hash of the signature.
type store struct {
	root  string
	quota int
}

func (s *store) ownerDir(owner string) string {
	return filepath.Join(s.root, "objects", owner)
}

func (s *store) path(owner, id string) string {
	return filepath.Join(s.ownerDir(owner), id+".json")
}

// put writes one envelope. An envelope that is already there is not written
// again: the same envelope answers as before, and a different one conflicts.
func (s *store) put(owner string, held *envelope) (map[string]any, error) {
	if err := os.MkdirAll(s.ownerDir(owner), 0o700); err != nil {
		return nil, errStorage
	}

	target := s.path(owner, held.ID)
	data, err := json.Marshal(held)
	if err != nil {
		return nil, errStorage
	}

	// A retry is checked before the quota, so an owner who is full may
	// always send an envelope that already landed.
	switch existing, err := os.ReadFile(target); {
	case err == nil:
		return same(existing, data, held)
	case !errors.Is(err, os.ErrNotExist):
		return nil, errCorrupt
	}

	if err := s.withinQuota(owner, len(data)); err != nil {
		return nil, err
	}

	existing, err := s.writeOnce(target, data)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return same(existing, data, held)
	}
	return map[string]any{"id": held.ID}, nil
}

// get reads one envelope, and checks it again before it leaves.
func (s *store) get(owner, id string) (map[string]any, error) {
	if !keyPattern.MatchString(id) {
		return nil, errInvalidRequest
	}

	info, err := os.Lstat(s.ownerDir(owner))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, errNotFound
	case err != nil || !info.IsDir():
		return nil, errCorrupt
	}

	data, err := os.ReadFile(s.path(owner, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, errCorrupt
	}

	held, err := validate(data, owner)
	if err != nil {
		return nil, errCorrupt
	}
	return map[string]any{"file": held}, nil
}

// list returns a page of the headers that one owner holds, in id order.
func (s *store) list(owner, cursor string) (map[string]any, error) {
	if cursor != "" && !keyPattern.MatchString(cursor) {
		return nil, errInvalidRequest
	}

	entries, err := os.ReadDir(s.ownerDir(owner))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{"files": []any{}, "next": nil}, nil
	}
	if err != nil {
		return nil, errCorrupt
	}

	var ids []string
	for _, entry := range entries {
		name := entry.Name()
		if len(name) != 69 || !strings.HasSuffix(name, ".json") {
			continue
		}
		if id := name[:64]; keyPattern.MatchString(id) && id > cursor {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	if len(ids) > 50 {
		ids = ids[:50]
	}

	headers := make([]any, 0, len(ids))
	for _, id := range ids {
		got, err := s.get(owner, id)
		if err != nil {
			return nil, errCorrupt
		}
		held, _ := got["file"].(*envelope)
		headers = append(headers, held.headers())
	}

	var next any
	if len(ids) == 50 {
		next = ids[len(ids)-1]
	}
	return map[string]any{"files": headers, "next": next}, nil
}

// withinQuota refuses a write that takes an owner over the budget.
func (s *store) withinQuota(owner string, bytes int) error {
	entries, err := os.ReadDir(s.ownerDir(owner))
	if err != nil {
		return errCorrupt
	}

	used := 0
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return errCorrupt
		}
		used += int(info.Size())
	}

	if used+bytes > s.quota {
		return errQuota
	}
	return nil
}

// writeOnce writes a file that never changes. It writes a new file and links
// it into place, so two writers at one time cannot lose a body. It returns
// the bytes that were there already, if any.
func (s *store) writeOnce(target string, data []byte) ([]byte, error) {
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return nil, errStorage
	}

	temporary := target + ".tmp-" + base64.RawURLEncoding.EncodeToString(suffix)
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, errStorage
	}

	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	file.Close()

	if writeErr != nil || syncErr != nil {
		os.Remove(temporary)
		return nil, errStorage
	}

	err = os.Link(temporary, target)
	os.Remove(temporary)

	switch {
	case err == nil:
		return nil, nil
	case errors.Is(err, os.ErrExist):
		existing, err := os.ReadFile(target)
		if err != nil {
			return nil, errStorage
		}
		return existing, nil
	default:
		return nil, errStorage
	}
}

// same answers a write of an envelope that is already there. The same
// envelope is not an error, and a different one is a conflict.
func same(existing, written []byte, held *envelope) (map[string]any, error) {
	var before, after any
	if err := json.Unmarshal(existing, &before); err != nil {
		return nil, errConflict
	}
	if err := json.Unmarshal(written, &after); err != nil {
		return nil, errConflict
	}

	first, _ := json.Marshal(before)
	second, _ := json.Marshal(after)
	if string(first) != string(second) {
		return nil, errConflict
	}
	return map[string]any{"id": held.ID}, nil
}
