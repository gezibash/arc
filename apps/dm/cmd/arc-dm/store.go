package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gezibash/arc/sdk/terminal"
)

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
	// prefixes counts the ids that start with each prefix of 6 and 7
	// digits, and buckets holds the ids of each prefix of 8, so a short id
	// takes no sort. Ids that share 8 digits are rare, unless a sender
	// made them; a short id compares only the ids of its bucket.
	prefixes map[string]int
	buckets  map[string][]string
	// ids are the ids, for a lookup and a long short id. They are sorted
	// unless unsorted.
	ids      []string
	unsorted bool
	// shown are the ids that this run showed as parents that are not on
	// this device, and avoid the ids that runs before it showed so. A
	// short id that starts one of them is refused (see lookup).
	shown   []string
	isShown map[string]bool
	avoid   map[string]bool
}

// shortLengths are the lengths of short id that prefixes count.
var shortLengths = []int{6, 7}

// bucketLength is the length of the prefix of a bucket.
const bucketLength = 8

func index(all []conversation) *store {
	s := &store{byID: map[string]held{}, prefixes: map[string]int{}, buckets: map[string][]string{}}
	for _, c := range all {
		for _, e := range c.Entries {
			s.add(e, c)
		}
	}
	return s
}

// add adds a message in constant time: a watch adds one at a time, and a
// flood of messages must not take time in the square of their number.
func (s *store) add(e entry, c conversation) {
	if _, ok := s.byID[e.ID]; ok {
		return
	}
	s.byID[e.ID] = held{entry: e, conv: c}
	s.ids = append(s.ids, e.ID)
	s.unsorted = true
	for _, n := range shortLengths {
		if len(e.ID) >= n {
			s.prefixes[e.ID[:n]]++
		}
	}
	if len(e.ID) >= bucketLength {
		s.buckets[e.ID[:bucketLength]] = append(s.buckets[e.ID[:bucketLength]], e.ID)
	}
}

// sorted returns the ids, sorted, for a lookup.
func (s *store) sorted() []string {
	if s.unsorted {
		slices.Sort(s.ids)
		s.unsorted = false
	}
	return s.ids
}

// short is the start of an id that names one message of the store: six hex
// digits, or more when another message starts with the same six.
func (s *store) short(id string) string {
	own := 0
	if _, ok := s.byID[id]; ok {
		own = 1
	}
	for _, n := range shortLengths {
		if len(id) >= n && s.prefixes[id[:n]] <= own {
			return id[:n]
		}
	}
	if len(id) < bucketLength {
		return id
	}
	n := 0
	for _, other := range s.buckets[id[:bucketLength]] {
		if other != id {
			n = max(n, commonPrefix(id, other))
		}
	}
	return id[:min(len(id), max(bucketLength, n+1))]
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
	if err := checkID(prefix); err != nil {
		return held{}, err
	}
	// A sender can name as a parent an id that starts as its own later
	// message, so a short id that arc-dm showed as missing could name that
	// message. Only the full id can name it then.
	for id := range s.avoid {
		if _, here := s.byID[id]; here {
			// The parent came later, or was a message of this citizen: the
			// short id names it.
			continue
		}
		if len(prefix) < len(id) && strings.HasPrefix(id, prefix) {
			return held{}, fmt.Errorf("%s was not on this device when arc-dm showed it, and could name another message now: give the full id", prefix)
		}
	}
	ids := s.sorted()
	i, _ := slices.BinarySearch(ids, prefix)
	var matches []string
	for ; i < len(ids) && strings.HasPrefix(ids[i], prefix); i++ {
		matches = append(matches, ids[i])
	}
	switch len(matches) {
	case 0:
		return held{}, fmt.Errorf("no message %s: run arc-dm open to see the ids", prefix)
	case 1:
		return s.byID[matches[0]], nil
	}
	return held{}, fmt.Errorf("%q names %d messages: give more digits", prefix, len(matches))
}

// checkID refuses a short id that is not at least 4 lowercase hex digits.
// A command checks it before it runs arc.
func checkID(prefix string) error {
	if len(prefix) < 4 || len(prefix) > 64 || strings.Trim(prefix, "0123456789abcdef") != "" {
		return fmt.Errorf("%q is not a message id: give at least 4 of its hex digits, as arc-dm open shows them", prefix)
	}
	return nil
}

// quoteWidth is the most columns of a quote line.
const quoteWidth = 72

// quote is the line that shows the parent of a reply in the conversation
// c, or "" for a message that is no reply. The text comes from the store,
// and only from the same conversation: a sender can name any id. The line
// starts at column 0, where no line of the text of a message can start.
func (s *store) quote(e entry, c conversation) string {
	if e.ReplyTo == "" {
		return ""
	}
	parent, ok := s.byID[e.ReplyTo]
	if !ok {
		// The id of a parent that is not here shows 12 digits: a message
		// that arrives later and starts with the same 6 must not take it.
		id := e.ReplyTo[:min(len(e.ReplyTo), quotedID)]
		if len(s.short(e.ReplyTo)) > quotedID {
			id += "…"
		}
		if !s.isShown[e.ReplyTo] {
			if s.isShown == nil {
				s.isShown = map[string]bool{}
			}
			s.isShown[e.ReplyTo] = true
			s.shown = append(s.shown, e.ReplyTo)
		}
		return "↳ " + id + "  (not on this device)"
	}
	head := "↳ " + s.quoted(e.ReplyTo) + "  "
	switch {
	case parent.conv.key() != c.key():
		return head + "(in another conversation)"
	}
	who := cmp.Or(parent.From, parent.conv.Name)
	if parent.Mine {
		who = "you"
	}
	// The cut counts columns, so wide text and a long name fit too. A
	// petname holds at most 32 characters, and a quoted id at most 13, so
	// the text gets at least 21 columns.
	return head + terminal.EscapeLineFit(who+": "+firstLine(parent.Text), quoteWidth-terminal.Width(head))
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
// text after it is left out. A text of only spaces, such as U+3000, is
// "(blank)": arc refuses to send one, but another client can.
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
	return "(blank)"
}
