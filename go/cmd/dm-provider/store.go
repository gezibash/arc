package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The mailbox of one citizen lives under the root:
//
//	mailboxes/<public key>/msgs/<id>.json
//	mailboxes/<public key>/blobs/<id>/<name>
//	mailboxes/<public key>/receipts.jsonl
//	mailboxes/<public key>/blocked
//	mailboxes/<public key>/muted
//	mailboxes/<public key>/settings.json
//	mailboxes/<public key>/usage
//
// Every body on disk is a sealed token. The store never holds plain text.

var publicKeyPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// store is one directory of mailboxes.
type store struct {
	root string
}

// message is one message as it lies on disk.
type message struct {
	ID          string     `json:"id"`
	From        string     `json:"from"`
	To          recipients `json:"to"`
	T           string     `json:"t"`
	Enc         string     `json:"enc"`
	Body        string     `json:"body"`
	ReplyTo     string     `json:"reply_to,omitempty"`
	Attachments []attach   `json:"attachments,omitempty"`
}

// recipients is the "to" field. A message of the first release named one
// recipient as a string, and a message of today names a list. Both read.
type recipients []string

// UnmarshalJSON reads a list, or one name.
func (r *recipients) UnmarshalJSON(data []byte) error {
	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*r = list
		return nil
	}

	var one string
	if err := json.Unmarshal(data, &one); err != nil {
		return err
	}
	*r = []string{one}
	return nil
}

// attach names one sealed attachment of a message.
type attach struct {
	Name  string `json:"name"`
	Bytes int    `json:"bytes"`
}

// receipt is one line of the receipts file.
type receipt struct {
	T     string `json:"t"`
	ID    string `json:"id"`
	Event string `json:"event"`
	By    string `json:"by,omitempty"`
	Value string `json:"value,omitempty"`
}

var defaultSettings = map[string]string{"receipts": "on"}

func isPublicKey(value string) bool { return publicKeyPattern.MatchString(value) }

func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

func (s *store) mailbox(key string) string   { return filepath.Join(s.root, "mailboxes", key) }
func (s *store) msgsDir(key string) string   { return filepath.Join(s.mailbox(key), "msgs") }
func (s *store) usagePath(key string) string { return filepath.Join(s.mailbox(key), "usage") }

func (s *store) msgPath(key, id string) string {
	return filepath.Join(s.msgsDir(key), id+".json")
}

func (s *store) blobsDir(key, id string) string {
	return filepath.Join(s.mailbox(key), "blobs", id)
}

func (s *store) receiptsPath(key string) string {
	return filepath.Join(s.mailbox(key), "receipts.jsonl")
}

// -- messages ---------------------------------------------------------------

// putMessage writes one message. With counted, the counter of the mailbox
// moves by the change in size.
func (s *store) putMessage(key string, msg *message, counted bool) (int, error) {
	if err := os.MkdirAll(s.msgsDir(key), 0o700); err != nil {
		return 0, err
	}

	data, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	return s.write(key, s.msgPath(key, msg.ID), data, counted)
}

func (s *store) getMessage(key, id string) (*message, error) {
	data, err := os.ReadFile(s.msgPath(key, id))
	if err != nil {
		return nil, errNotFound
	}

	msg := &message{}
	if err := json.Unmarshal(data, msg); err != nil {
		return nil, errNotFound
	}
	return msg, nil
}

// listMessages returns every message of a mailbox, oldest first. The ids
// sort by time.
func (s *store) listMessages(key string) []*message {
	entries, err := os.ReadDir(s.msgsDir(key))
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	messages := make([]*message, 0, len(names))
	for _, name := range names {
		data, err := os.ReadFile(filepath.Join(s.msgsDir(key), name))
		if err != nil {
			continue
		}
		msg := &message{}
		if err := json.Unmarshal(data, msg); err != nil {
			continue
		}
		messages = append(messages, msg)
	}
	return messages
}

// removeMessage takes one message and its attachments off the disk, and
// returns the bytes that left. It does not touch the counter.
func (s *store) removeMessage(key, id string) int {
	before := s.fileSize(s.msgPath(key, id)) + s.treeSize(s.blobsDir(key, id))
	os.Remove(s.msgPath(key, id))
	os.RemoveAll(s.blobsDir(key, id))
	after := s.fileSize(s.msgPath(key, id)) + s.treeSize(s.blobsDir(key, id))
	return before - after
}

// purge takes the messages below an id off the disk, and returns how many
// went.
func (s *store) purge(key, before string) int {
	freed, count := 0, 0
	for _, msg := range s.listMessages(key) {
		if msg.ID < before {
			freed += s.removeMessage(key, msg.ID)
			count++
		}
	}
	s.bumpUsage(key, -freed)
	return count
}

// -- attachments ------------------------------------------------------------

func (s *store) putBlob(key, id, name, token string, counted bool) (int, error) {
	if err := os.MkdirAll(s.blobsDir(key, id), 0o700); err != nil {
		return 0, err
	}
	return s.write(key, filepath.Join(s.blobsDir(key, id), name), []byte(token), counted)
}

func (s *store) getBlob(key, id, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.blobsDir(key, id), name))
	if err != nil {
		return "", errNotFound
	}
	return string(data), nil
}

// -- budget -----------------------------------------------------------------

// usage is the bytes that a mailbox holds. A counter that is missing or
// broken is rebuilt from a walk of the mailbox.
func (s *store) usage(key string) int {
	data, err := os.ReadFile(s.usagePath(key))
	if err == nil {
		if used, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && used >= 0 {
			return used
		}
	}
	return s.recountUsage(key)
}

// recountUsage walks a mailbox, saves its byte count, and returns it.
func (s *store) recountUsage(key string) int {
	os.MkdirAll(s.mailbox(key), 0o700)

	used := s.treeSize(s.msgsDir(key)) + s.treeSize(filepath.Join(s.mailbox(key), "blobs"))
	s.writeUsage(key, used)
	return used
}

// bumpUsage moves the counter of a mailbox. A result below zero starts a
// recount.
func (s *store) bumpUsage(key string, delta int) {
	if delta == 0 {
		return
	}
	if used := s.usage(key) + delta; used >= 0 {
		s.writeUsage(key, used)
		return
	}
	s.recountUsage(key)
}

func (s *store) writeUsage(key string, used int) {
	os.MkdirAll(s.mailbox(key), 0o700)

	path := s.usagePath(key)
	if err := os.WriteFile(path+".tmp", []byte(strconv.Itoa(used)), 0o600); err != nil {
		return
	}
	os.Rename(path+".tmp", path)
}

// write saves one file. With counted, the counter moves by the change in
// size. The counter is read before the write, so a first walk leaves the new
// file out.
func (s *store) write(key, path string, data []byte, counted bool) (int, error) {
	if !counted {
		return len(data), os.WriteFile(path, data, 0o600)
	}

	base := s.usage(key)
	before := s.fileSize(path)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return 0, err
	}
	s.writeUsage(key, base+len(data)-before)
	return len(data), nil
}

func (s *store) fileSize(path string) int {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return 0
	}
	return int(info.Size())
}

func (s *store) treeSize(dir string) int {
	total := 0
	filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return nil
		}
		if info, err := entry.Info(); err == nil {
			total += int(info.Size())
		}
		return nil
	})
	return total
}

// -- receipts ---------------------------------------------------------------

func (s *store) addReceipt(key, id, event string, extra receipt) error {
	if err := os.MkdirAll(s.mailbox(key), 0o700); err != nil {
		return err
	}

	extra.T = timestamp()
	extra.ID = id
	extra.Event = event

	line, err := json.Marshal(extra)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(s.receiptsPath(key), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()

	_, err = file.Write(append(line, '\n'))
	return err
}

// allReceipts returns every receipt of a mailbox, in file order.
func (s *store) allReceipts(key string) []receipt {
	data, err := os.ReadFile(s.receiptsPath(key))
	if err != nil {
		return nil
	}

	var receipts []receipt
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		one := receipt{}
		if err := json.Unmarshal([]byte(line), &one); err == nil {
			receipts = append(receipts, one)
		}
	}
	return receipts
}

func (s *store) receiptsOf(key, id string) []receipt {
	var found []receipt
	for _, one := range s.allReceipts(key) {
		if one.ID == id {
			found = append(found, one)
		}
	}
	return found
}

// receiptIndex maps a message id to the events that it holds.
func receiptIndex(receipts []receipt) map[string]map[string]bool {
	index := map[string]map[string]bool{}
	for _, one := range receipts {
		if index[one.ID] == nil {
			index[one.ID] = map[string]bool{}
		}
		index[one.ID][one.Event] = true
	}
	return index
}

func (s *store) receiptIndex(key string) map[string]map[string]bool {
	return receiptIndex(s.allReceipts(key))
}

// reaction is one reaction that stands right now.
type reaction struct {
	By    string
	Value string
}

// reactionIndex maps a message id to the reactions that stand. The last
// reaction of one citizen wins, and an empty value clears it.
func reactionIndex(receipts []receipt) map[string][]reaction {
	latest := map[string]map[string]string{}

	for _, one := range receipts {
		if one.Event != "reaction" {
			continue
		}
		if latest[one.ID] == nil {
			latest[one.ID] = map[string]string{}
		}
		latest[one.ID][one.By] = one.Value
	}

	index := map[string][]reaction{}
	for id, byKey := range latest {
		var current []reaction
		for by, value := range byKey {
			if value != "" {
				current = append(current, reaction{By: by, Value: value})
			}
		}
		sort.Slice(current, func(left, right int) bool { return current[left].By < current[right].By })
		index[id] = current
	}
	return index
}

// -- lists and settings ------------------------------------------------------

func (s *store) blocked(key string) []string {
	return s.readLines(filepath.Join(s.mailbox(key), "blocked"))
}
func (s *store) muted(key string) []string {
	return s.readLines(filepath.Join(s.mailbox(key), "muted"))
}

func (s *store) writeBlocked(key string, keys []string) error {
	return s.writeLines(filepath.Join(s.mailbox(key), "blocked"), keys)
}

func (s *store) writeMuted(key string, keys []string) error {
	return s.writeLines(filepath.Join(s.mailbox(key), "muted"), keys)
}

func (s *store) isBlocked(key, sender string) bool {
	for _, held := range s.blocked(key) {
		if held == sender {
			return true
		}
	}
	return false
}

func (s *store) settings(key string) map[string]string {
	held := map[string]string{}
	for name, value := range defaultSettings {
		held[name] = value
	}

	data, err := os.ReadFile(filepath.Join(s.mailbox(key), "settings.json"))
	if err != nil {
		return held
	}

	var saved map[string]string
	if err := json.Unmarshal(data, &saved); err != nil {
		return held
	}
	for name, value := range saved {
		held[name] = value
	}
	return held
}

func (s *store) writeSettings(key string, settings map[string]string) error {
	if err := os.MkdirAll(s.mailbox(key), 0o700); err != nil {
		return err
	}

	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.mailbox(key), "settings.json"), data, 0o600)
}

func (s *store) readLines(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	var lines []string
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func (s *store) writeLines(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	var body strings.Builder
	for _, line := range lines {
		body.WriteString(line)
		body.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(body.String()), 0o600)
}

var errNotFound = errors.New("not_found")

func errorf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
