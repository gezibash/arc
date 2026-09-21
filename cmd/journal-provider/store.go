package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gezibash/arc/provider"
)

// The journal lives under one root:
//
//	repo/projects/<project>/ACL
//	repo/projects/<project>/<notebook>/<page>.md
//	repo/projects/<project>/<notebook>/kpi.jsonl
//	blobs/<sha256>
//
// The repo is a git repository. Every write is one commit in the name of the
// caller.

// MaxBlobBytes is the largest attachment. It is a budget of the disk, not a
// limit of the transport: a blob lives outside git and is not backed up.
const MaxBlobBytes = 16 * 1024 * 1024

// The errors that a caller may see.
const (
	errInvalidAddress = provider.Error("invalid_address")
	errNotFound       = provider.Error("not_found")
	errForbidden      = provider.Error("forbidden")
	errStorage        = provider.Error("storage_failure")
)

var segmentPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type store struct {
	root string
	// afterCommit runs after every commit. It starts the reindex and the
	// push, each after its own pause.
	afterCommit func()
}

func (s *store) repo() string        { return filepath.Join(s.root, "repo") }
func (s *store) projectsDir() string { return filepath.Join(s.repo(), "projects") }

// ensure makes the directories and the repository.
func (s *store) ensure() error {
	if err := os.MkdirAll(filepath.Join(s.root, "blobs"), 0o700); err != nil {
		return errStorage
	}
	if err := os.MkdirAll(s.projectsDir(), 0o700); err != nil {
		return errStorage
	}
	return s.gitInit()
}

// -- addresses --------------------------------------------------------------

// address splits an address and checks that it holds the right number of
// segments.
func address(raw string, count int) ([]string, error) {
	var parts []string
	for _, part := range strings.Split(raw, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}

	if len(parts) != count {
		return nil, errInvalidAddress
	}
	for _, part := range parts {
		if !segmentPattern.MatchString(part) {
			return nil, errInvalidAddress
		}
	}
	return parts, nil
}

func pagePath(parts []string) string {
	return filepath.Join("projects", parts[0], parts[1], parts[2]+".md")
}

func kpiPath(parts []string) string {
	return filepath.Join("projects", parts[0], parts[1], "kpi.jsonl")
}

func aclPath(project string) string {
	return filepath.Join("projects", project, "ACL")
}

// -- the access list --------------------------------------------------------

// acl returns the keys that may reach a project. The first key is the owner.
func (s *store) acl(project string) []string {
	data, err := os.ReadFile(filepath.Join(s.repo(), aclPath(project)))
	if err != nil {
		return nil
	}

	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			keys = append(keys, line)
		}
	}
	return keys
}

func (s *store) owner(project string) string {
	if keys := s.acl(project); len(keys) > 0 {
		return keys[0]
	}
	return ""
}

func (s *store) allowed(project, from string) bool {
	for _, key := range s.acl(project) {
		if key == from {
			return true
		}
	}
	return false
}

func (s *store) projectExists(project string) bool {
	info, err := os.Stat(filepath.Join(s.projectsDir(), project))
	return err == nil && info.IsDir()
}

// authorizeWrite makes the project when it is not there, with the caller as
// its owner.
func (s *store) authorizeWrite(project, from string) error {
	switch {
	case !s.projectExists(project):
		return s.writeACL(project, []string{from}, from, "create project "+project)
	case s.allowed(project, from):
		return nil
	default:
		return errForbidden
	}
}

func (s *store) authorizeRead(project, from string) error {
	if s.allowed(project, from) {
		return nil
	}
	return errForbidden
}

func (s *store) writeACL(project string, keys []string, from, message string) error {
	path := aclPath(project)
	full := filepath.Join(s.repo(), path)

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return errStorage
	}
	if err := os.WriteFile(full, []byte(strings.Join(keys, "\n")+"\n"), 0o600); err != nil {
		return errStorage
	}
	return s.commit([]string{path}, from, message)
}

// -- pages ------------------------------------------------------------------

func (s *store) readPage(parts []string) (*page, string, error) {
	path := pagePath(parts)

	data, err := os.ReadFile(filepath.Join(s.repo(), path))
	if err != nil {
		return nil, "", errNotFound
	}
	return parsePage(string(data)), s.rev(path), nil
}

// writePage saves a page and commits it. It sets the author once, and the
// time of the change on every write.
func (s *store) writePage(parts []string, held *page, from, message string) (string, error) {
	path := pagePath(parts)
	full := filepath.Join(s.repo(), path)
	now := timestamp()

	if held.Meta == nil {
		held.Meta = map[string]any{}
	}
	if _, set := held.Meta["created"]; !set {
		held.Meta["created"] = now
	}
	if _, set := held.Meta["author"]; !set {
		held.Meta["author"] = from
	}
	held.Meta["updated"] = now

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return "", errStorage
	}
	if err := os.WriteFile(full, []byte(held.render()), 0o600); err != nil {
		return "", errStorage
	}
	if err := s.commit([]string{path}, from, message); err != nil {
		return "", err
	}
	return s.rev(path), nil
}

// checkRev refuses a write when the page moved since the caller read it.
func (s *store) checkRev(parts []string, expected string) error {
	if expected == "" {
		return nil
	}

	current := s.rev(pagePath(parts))
	if current == expected {
		return nil
	}
	if current == "" {
		current = "none"
	}
	return provider.Error("conflict current rev: " + current)
}

// -- listing ----------------------------------------------------------------

func (s *store) listProjects(from string) []string {
	var projects []string
	for _, name := range directories(s.projectsDir()) {
		if s.allowed(name, from) {
			projects = append(projects, name)
		}
	}
	return projects
}

func (s *store) listNotebooks(project string) []string {
	return directories(filepath.Join(s.projectsDir(), project))
}

// listPages names each page of a notebook with its title.
func (s *store) listPages(project, notebook string) [][2]string {
	dir := filepath.Join(s.projectsDir(), project, notebook)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var names []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".md") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	pages := make([][2]string, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}

		title, _ := parsePage(string(data)).Meta["title"].(string)
		page := strings.TrimSuffix(name, ".md")
		pages = append(pages, [2]string{project + "/" + notebook + "/" + page, title})
	}
	return pages
}

func directories(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// -- the numbers ------------------------------------------------------------

// kpi is one measurement of a notebook.
type kpi struct {
	T     string  `json:"t"`
	By    string  `json:"by"`
	Key   string  `json:"key"`
	Value float64 `json:"value"`
	Ref   string  `json:"ref,omitempty"`
	Note  string  `json:"note,omitempty"`
	whole bool
}

// MarshalJSON writes a whole number without a fraction, as the first
// implementation does.
func (k kpi) MarshalJSON() ([]byte, error) {
	fields := map[string]any{"t": k.T, "by": k.By, "key": k.Key}
	if k.whole {
		fields["value"] = int64(k.Value)
	} else {
		fields["value"] = k.Value
	}
	if k.Ref != "" {
		fields["ref"] = k.Ref
	}
	if k.Note != "" {
		fields["note"] = k.Note
	}
	return json.Marshal(fields)
}

func (k kpi) value() string {
	if k.Value == float64(int64(k.Value)) {
		return fmt.Sprintf("%d", int64(k.Value))
	}
	return fmt.Sprintf("%g", k.Value)
}

// format writes one measurement for a person to read.
func (k kpi) format() string {
	out := fmt.Sprintf("%s  %s=%s  by=%s", k.T, k.Key, k.value(), cut(k.By, 12))
	if k.Ref != "" {
		out += "  ref=" + k.Ref
	}
	if k.Note != "" {
		out += "  note=" + k.Note
	}
	return out
}

func (s *store) kpiFile(parts []string) string {
	return filepath.Join(s.repo(), kpiPath(parts))
}

func (s *store) kpiAppend(parts []string, record kpi, from string) error {
	path := s.kpiFile(parts)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return errStorage
	}

	line, err := json.Marshal(record)
	if err != nil {
		return errStorage
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return errStorage
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		file.Close()
		return errStorage
	}
	file.Close()

	message := fmt.Sprintf("kpi %s %s=%s", strings.Join(parts, "/"), record.Key, record.value())
	return s.commit([]string{kpiPath(parts)}, from, message)
}

func (s *store) kpiRead(parts []string) []kpi {
	data, err := os.ReadFile(s.kpiFile(parts))
	if err != nil {
		return nil
	}

	var records []kpi
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		one := kpi{}
		if err := json.Unmarshal([]byte(line), &one); err == nil {
			records = append(records, one)
		}
	}
	return records
}

// kpiLatest keeps the last measurement of each key, in key order.
func (s *store) kpiLatest(parts []string) []kpi {
	latest := map[string]kpi{}
	for _, one := range s.kpiRead(parts) {
		latest[one.Key] = one
	}

	keys := make([]string, 0, len(latest))
	for key := range latest {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	records := make([]kpi, 0, len(keys))
	for _, key := range keys {
		records = append(records, latest[key])
	}
	return records
}

// -- attachments ------------------------------------------------------------

// putBlob saves one attachment under the hash of its bytes.
func (s *store) putBlob(data []byte) (string, error) {
	digest := sha256.Sum256(data)
	name := hex.EncodeToString(digest[:])
	path := filepath.Join(s.root, "blobs", name)

	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return "", errStorage
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			return "", errStorage
		}
	}
	return name, nil
}

func (s *store) getBlob(name string) ([]byte, error) {
	if !shaPattern.MatchString(name) {
		return nil, errNotFound
	}

	data, err := os.ReadFile(filepath.Join(s.root, "blobs", name))
	if err != nil {
		return nil, errNotFound
	}
	return data, nil
}

// -- git --------------------------------------------------------------------

func (s *store) gitInit() error {
	if info, err := os.Stat(filepath.Join(s.repo(), ".git")); err == nil && info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(s.repo(), 0o700); err != nil {
		return errStorage
	}

	settings := [][]string{
		{"init", "-q", "-b", "main"},
		{"config", "user.email", "journal@arc"},
		{"config", "user.name", "journal"},
		// This is a repository of data. The hooks of the user never run here.
		{"config", "core.hooksPath", "/dev/null"},
	}

	for _, args := range settings {
		if _, err := s.git(args...); err != nil {
			return errStorage
		}
	}
	return nil
}

// commit stages the paths and commits them in the name of the caller.
func (s *store) commit(paths []string, from, message string) error {
	if _, err := s.git(append([]string{"add", "--"}, paths...)...); err != nil {
		return errStorage
	}

	author := fmt.Sprintf("%s <%s@arc>", cut(from, 12), from)
	if _, err := s.git("commit", "-q", "--allow-empty", "--author", author, "-m", message); err != nil {
		return errStorage
	}

	if s.afterCommit != nil {
		s.afterCommit()
	}
	return nil
}

// rev is the short name of the last commit that touched a path.
func (s *store) rev(path string) string {
	out, err := s.git("log", "-n1", "--format=%h", "--", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (s *store) history(path string) string {
	out, err := s.git("log", "--format=%h %aI %an", "--", path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (s *store) push(remote string) error {
	if _, err := s.git("remote", "get-url", "origin"); err != nil {
		if _, err := s.git("remote", "add", "origin", remote); err != nil {
			return errStorage
		}
	}

	if _, err := s.git("push", "-q", "-u", "origin", "main"); err != nil {
		return err
	}
	return nil
}

func (s *store) git(args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = s.repo()
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")

	out, err := command.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %s", args[0], strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

func cut(value string, length int) string {
	if len(value) <= length {
		return value
	}
	return value[:length]
}
