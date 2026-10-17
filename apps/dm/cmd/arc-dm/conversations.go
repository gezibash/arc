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
	"unicode/utf8"

	"github.com/gezibash/arc/sdk/arcprogram"
	"github.com/gezibash/arc/sdk/atomicfile"
	"github.com/gezibash/arc/sdk/terminal"
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
	// ReplyTo is the id of the parent of a reply, as its sender chose it.
	ReplyTo string
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
		add(members, entry{ID: m.ID, At: m.At, Text: m.Text, From: m.Name, ReplyTo: m.ReplyTo})
	}
	for _, m := range sentMessages(outbox) {
		add(m.members, m.entry)
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

// sentMessage is one message that this citizen sent, and its members.
type sentMessage struct {
	members []string
	entry   entry
}

// sentMessages joins the sent lines of the outbox into messages. A group
// message has one sent line for each recipient, with one id.
func sentMessages(outbox []arcprogram.Sent) []sentMessage {
	var order []string
	sent := map[string][]arcprogram.Sent{}
	for _, s := range outbox {
		if sent[s.ID] == nil {
			order = append(order, s.ID)
		}
		sent[s.ID] = append(sent[s.ID], s)
	}
	out := make([]sentMessage, 0, len(order))
	for _, id := range order {
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
		out = append(out, sentMessage{members, entry{ID: id, At: lines[0].At, Text: lines[0].Text, Mine: true, State: combined(states), ReplyTo: lines[0].ReplyTo}})
	}
	return out
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

// sharedGroup is the group that the peer of an empty direct conversation
// shares with this citizen, when there is exactly one. Else it is c.
func sharedGroup(all []conversation, c conversation) conversation {
	if !c.direct() || len(c.Entries) > 0 {
		return c
	}
	var groups []conversation
	for _, g := range all {
		if !g.direct() && slices.Contains(g.Peers, c.Peers[0]) {
			groups = append(groups, g)
		}
	}
	if len(groups) == 1 {
		return groups[0]
	}
	return c
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

// held is one message of this machine, and the conversation that holds it.
type held struct {
	entry
	conv conversation
}

// store holds the messages of this machine by id: each conversation, sent
// and received. A reply shows the text of its parent only from here, never
// from the reply.
type store struct {
	byID map[string]held
	// ids are the ids, sorted, for the short ids.
	ids []string
}

func index(all []conversation) *store {
	s := &store{byID: map[string]held{}}
	for _, c := range all {
		for _, e := range c.Entries {
			s.add(e, c)
		}
	}
	return s
}

func (s *store) add(e entry, c conversation) {
	if _, ok := s.byID[e.ID]; ok {
		return
	}
	s.byID[e.ID] = held{entry: e, conv: c}
	i, _ := slices.BinarySearch(s.ids, e.ID)
	s.ids = slices.Insert(s.ids, i, e.ID)
}

// short is the start of an id that names one message of the store: six hex
// digits, or more when another message starts with the same six.
func (s *store) short(id string) string {
	n := 0
	i, _ := slices.BinarySearch(s.ids, id)
	for _, j := range []int{i - 1, i, i + 1} {
		if j >= 0 && j < len(s.ids) && s.ids[j] != id {
			n = max(n, commonPrefix(id, s.ids[j]))
		}
	}
	return id[:min(len(id), max(6, n+1))]
}

func commonPrefix(a, b string) int {
	n := 0
	for n < len(a) && n < len(b) && a[n] == b[n] {
		n++
	}
	return n
}

// lookup finds the message whose id starts with prefix: at least four hex
// digits. It refuses a prefix that names more than one message.
func (s *store) lookup(prefix string) (held, error) {
	if len(prefix) < 4 || len(prefix) > 64 || strings.Trim(prefix, "0123456789abcdef") != "" {
		return held{}, fmt.Errorf("%q is not a message id: give at least 4 of its hex digits, as arc-dm open shows them", prefix)
	}
	i, _ := slices.BinarySearch(s.ids, prefix)
	var matches []string
	for ; i < len(s.ids) && strings.HasPrefix(s.ids[i], prefix); i++ {
		matches = append(matches, s.ids[i])
	}
	switch len(matches) {
	case 0:
		return held{}, fmt.Errorf("no message %s: run arc-dm open to see the ids", prefix)
	case 1:
		return s.byID[matches[0]], nil
	}
	return held{}, fmt.Errorf("%q names %d messages: give more digits", prefix, len(matches))
}

// quoteWidth is the most characters of a quote line.
const quoteWidth = 72

// quote is the line that shows the parent of a reply in the conversation
// c, or "" for a message that is no reply. The text comes from the store,
// and only from the same conversation: a sender can name any id. The line
// starts at column 0, where no line of the text of a message can start.
func (s *store) quote(e entry, c conversation) string {
	if e.ReplyTo == "" {
		return ""
	}
	head := "↳ " + s.quoted(e.ReplyTo) + "  "
	parent, ok := s.byID[e.ReplyTo]
	switch {
	case !ok:
		return head + "(not on this device)"
	case parent.conv.key() != c.key():
		return head + "(in another conversation)"
	}
	who := cmp.Or(parent.From, parent.conv.Name)
	if parent.Mine {
		who = "you"
	}
	head += who + ": "
	// A petname holds at most 32 characters, and a quoted id at most 13,
	// so the text gets at least 21.
	return head + terminal.EscapeLineCut(firstLine(parent.Text), max(1, quoteWidth-utf8.RuneCountInString(head)))
}

// quotedID is the most hex digits of an id on a quote line.
const quotedID = 12

// quoted is the short id of a parent on a quote line: at most 12 digits,
// then an ellipsis. A sender can name an id that matches a held id in 63
// digits, and its short id would fill the line. The ellipsis keeps it
// apart from each short id of a header, which has none.
func (s *store) quoted(id string) string {
	if short := s.short(id); len(short) <= quotedID {
		return short
	}
	return id[:quotedID] + "…"
}

// firstLine is the first line of text that is not empty, and " …" when
// text after it is left out.
func firstLine(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if strings.TrimSpace(strings.Join(lines[i+1:], "\n")) != "" {
			return line + " …"
		}
		return line
	}
	return ""
}
