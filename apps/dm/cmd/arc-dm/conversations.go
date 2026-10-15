package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/atomicfile"
)

// entry is one message of a conversation, received or sent.
type entry struct {
	ID   string
	At   time.Time
	Text string
	// Mine is true for a message that this citizen sent. State is then
	// pending, delivered or expired.
	Mine  bool
	State string
}

// conversation is the messages between this citizen and one peer, oldest
// first.
type conversation struct {
	Peer    string
	Name    string
	Entries []entry
}

// last is the newest message of the conversation.
func (c conversation) last() entry { return c.Entries[len(c.Entries)-1] }

// unread counts the received messages that are not in read.
func (c conversation) unread(read readSet) int {
	n := 0
	for _, e := range c.Entries {
		if !e.Mine && !read[e.ID] {
			n++
		}
	}
	return n
}

// conversations asks arc for the inbox and the outbox, and groups the
// messages by peer. The conversation with the newest message comes first.
func conversations(ctx context.Context, arc arcprogram.Arc) ([]conversation, error) {
	inbox, err := arc.Inbox(ctx)
	if err != nil {
		return nil, err
	}
	outbox, err := arc.Outbox(ctx)
	if err != nil {
		return nil, err
	}
	byPeer := map[string]*conversation{}
	add := func(peer, name string, e entry) {
		c := byPeer[peer]
		if c == nil {
			c = &conversation{Peer: peer, Name: name}
			byPeer[peer] = c
		}
		c.Entries = append(c.Entries, e)
	}
	for _, m := range inbox {
		add(m.From, m.Name, entry{ID: m.ID, At: m.At, Text: m.Text})
	}
	for _, s := range outbox {
		add(s.To, s.Name, entry{ID: s.ID, At: s.At, Text: s.Text, Mine: true, State: s.State})
	}
	var out []conversation
	for _, c := range byPeer {
		// The inbox comes before the outbox, so a reply in the same second
		// as the message comes after it.
		slices.SortStableFunc(c.Entries, func(a, b entry) int { return a.At.Compare(b.At) })
		out = append(out, *c)
	}
	slices.SortFunc(out, func(a, b conversation) int {
		return cmp.Or(b.last().At.Compare(a.last().At), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// find selects the conversation of a peer: a public key, a petname, or the
// start of either. A public key with no conversation gives a new one.
func find(all []conversation, peer string) (conversation, error) {
	var matches []conversation
	for _, c := range all {
		if c.Peer == peer || c.Name == peer {
			return c, nil
		}
		if strings.HasPrefix(c.Peer, peer) || strings.HasPrefix(c.Name, peer) {
			matches = append(matches, c)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		names := make([]string, len(matches))
		for i, c := range matches {
			names[i] = c.Name
		}
		return conversation{}, fmt.Errorf("%q names %d conversations: %s", peer, len(matches), strings.Join(names, ", "))
	case arcprogram.IsKey(peer):
		return conversation{Peer: peer}, nil
	default:
		return conversation{}, fmt.Errorf("no conversation with %q: give the public key of 64 hex digits", peer)
	}
}

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
