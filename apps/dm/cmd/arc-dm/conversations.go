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
	// From is the petname of the sender of a received message.
	From string
	// Mine is true for a message that this citizen sent. State is then
	// pending, delivered or expired, or for a group "delivered to 1 of 2".
	Mine  bool
	State string
}

// conversation is the messages between this citizen and a set of peers,
// oldest first. A direct conversation has one peer. A change of members
// makes a new conversation, as NIP-17 says.
type conversation struct {
	// Peers are the public keys of the other members, sorted, and Names
	// their petnames, in the same order.
	Peers []string
	Names []string
	// Name is the petnames, separated by commas.
	Name    string
	Entries []entry
}

func (c conversation) key() string  { return strings.Join(c.Peers, ",") }
func (c conversation) direct() bool { return len(c.Peers) == 1 }

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
// messages by the set of their members. The conversation with the newest
// message comes first.
func conversations(ctx context.Context, arc arcprogram.Arc) ([]conversation, error) {
	me, err := arc.PublicKey(ctx)
	if err != nil {
		return nil, err
	}
	inbox, err := arc.Inbox(ctx)
	if err != nil {
		return nil, err
	}
	outbox, err := arc.Outbox(ctx)
	if err != nil {
		return nil, err
	}
	// arc prints the petname of the sender and of each recipient. A member
	// with no message to or from this citizen gets the petname of its key.
	names := map[string]string{}
	for _, m := range inbox {
		names[m.From] = m.Name
	}
	for _, s := range outbox {
		names[s.To] = s.Name
	}

	byKey := map[string]*conversation{}
	add := func(members []string, e entry) {
		peers := peersOf(me, members)
		c := byKey[strings.Join(peers, ",")]
		if c == nil {
			c = newConversation(peers, names)
			byKey[c.key()] = c
		}
		c.Entries = append(c.Entries, e)
	}
	for _, m := range inbox {
		members := m.Members
		if members == nil {
			// An arc older than group messages prints no members.
			members = []string{m.From}
		}
		add(members, entry{ID: m.ID, At: m.At, Text: m.Text, From: m.Name})
	}
	// A group message has one sent line for each recipient, with one id.
	var sentOrder []string
	sent := map[string][]arcprogram.Sent{}
	for _, s := range outbox {
		if sent[s.ID] == nil {
			sentOrder = append(sentOrder, s.ID)
		}
		sent[s.ID] = append(sent[s.ID], s)
	}
	for _, id := range sentOrder {
		lines := sent[id]
		members := lines[0].Members
		if members == nil {
			for _, s := range lines {
				members = append(members, s.To)
			}
		}
		states := make([]string, len(lines))
		for i, s := range lines {
			states[i] = s.State
		}
		add(members, entry{ID: id, At: lines[0].At, Text: lines[0].Text, Mine: true, State: combined(states)})
	}

	var out []conversation
	for _, c := range byKey {
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

// peersOf is the members other than me, sorted, once each.
func peersOf(me string, members []string) []string {
	var peers []string
	for _, key := range members {
		if key != me && !slices.Contains(peers, key) {
			peers = append(peers, key)
		}
	}
	slices.Sort(peers)
	return peers
}

func newConversation(peers []string, names map[string]string) *conversation {
	c := &conversation{Peers: peers}
	for _, key := range peers {
		c.Names = append(c.Names, cmp.Or(names[key], arcprogram.Petname(key), key[:min(8, len(key))]))
	}
	c.Name = strings.Join(c.Names, ", ")
	return c
}

// combined is the state of a sent message from the state of each
// recipient.
func combined(states []string) string {
	if len(states) == 1 {
		return states[0]
	}
	delivered, expired := 0, 0
	for _, s := range states {
		switch s {
		case "delivered":
			delivered++
		case "expired":
			expired++
		}
	}
	switch {
	case delivered == len(states):
		return "delivered"
	case expired == len(states):
		return "expired"
	case delivered == 0 && expired == 0:
		return "pending"
	}
	return fmt.Sprintf("delivered to %d of %d", delivered, len(states))
}

// find selects a conversation. A peer is a public key or a petname, the
// start of either, or the end of a petname, and selects the direct
// conversation with that citizen. A member of a group with no direct
// conversation selects a new one. Several peers, separated by commas,
// select the group of those members. A set of keys with no conversation
// gives a new one.
func find(all []conversation, peer string) (conversation, error) {
	if strings.Contains(peer, ",") {
		return findGroup(all, peer)
	}
	var matches []conversation
	for _, c := range all {
		if !c.direct() {
			continue
		}
		if c.Peers[0] == peer || c.Name == peer {
			return c, nil
		}
		if partial(c.Peers[0], c.Name, peer) {
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
	}
	// No direct conversation: the peer can be a member of a group.
	names := known(all)
	key, err := resolve(names, peer)
	if err != nil {
		return conversation{}, fmt.Errorf("no conversation with %q: give a petname or key from arc-dm ls, or the public key of 64 hex digits", peer)
	}
	return *newConversation([]string{key}, names), nil
}

// partial says whether peer is the start of a key or a petname, or the
// start of the hex digits at the end of a petname.
func partial(key, name, peer string) bool {
	if strings.HasPrefix(key, peer) || strings.HasPrefix(name, peer) {
		return true
	}
	i := strings.LastIndex(name, "-")
	return i >= 0 && strings.HasPrefix(name[i+1:], peer)
}

// known maps the key of each peer of each conversation to its petname.
func known(all []conversation) map[string]string {
	names := map[string]string{}
	for _, c := range all {
		for i, key := range c.Peers {
			names[key] = c.Names[i]
		}
	}
	return names
}

// noMessages says that a conversation has no messages yet, and names the
// groups that its peer shares with this citizen.
func noMessages(all []conversation, c conversation) string {
	if !c.direct() {
		return "no messages"
	}
	var groups []string
	for _, g := range all {
		if !g.direct() && slices.Contains(g.Peers, c.Peers[0]) {
			groups = append(groups, "  arc-dm open "+strings.Join(g.Names, ","))
		}
	}
	if len(groups) == 0 {
		return "no messages with " + c.Name
	}
	return fmt.Sprintf("no direct messages with %s; you share %d group(s) with them:\n%s", c.Name, len(groups), strings.Join(groups, "\n"))
}

// findGroup selects the conversation of several peers. Each peer is a
// public key, or the petname or the start of a member of a known
// conversation.
func findGroup(all []conversation, text string) (conversation, error) {
	names := known(all)
	var peers []string
	for part := range strings.SplitSeq(text, ",") {
		part = strings.TrimSpace(part)
		key, err := resolve(names, part)
		if err != nil {
			return conversation{}, err
		}
		if !slices.Contains(peers, key) {
			peers = append(peers, key)
		}
	}
	slices.Sort(peers)
	for _, c := range all {
		if slices.Equal(c.Peers, peers) {
			return c, nil
		}
	}
	return *newConversation(peers, names), nil
}

// resolve finds the key of one peer among known names.
func resolve(names map[string]string, peer string) (string, error) {
	if peer == "" {
		return "", errors.New("a group names each peer: an empty peer is not one")
	}
	var matches []string
	for key, name := range names {
		if key == peer || name == peer {
			return key, nil
		}
		if partial(key, name, peer) {
			matches = append(matches, key)
		}
	}
	switch {
	case len(matches) == 1:
		return matches[0], nil
	case len(matches) > 1:
		return "", fmt.Errorf("%q names %d peers", peer, len(matches))
	case arcprogram.IsKey(peer):
		return peer, nil
	default:
		return "", fmt.Errorf("no peer %q: give the public key of 64 hex digits", peer)
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
