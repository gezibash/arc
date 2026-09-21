// Package journal is a private notebook on the delivery layer.
//
// A page is a head and its parts. Every event is signed by the owner, and its
// content is sealed to the owner with NIP-44, so a relay or a USB stick holds
// only ciphertext.
//
//	head   kind 30078, addressable. The d tag is "arc-journal:" and an HMAC of
//	       the address, so the address never leaves the machine. The sealed
//	       content names the address, the title, and the parts in order.
//	part   kind 3275. At most 32 KiB of the page text. Its a tag names the
//	       head it belongs to.
//
// A relay caps the size of one event, so a large page travels as many parts.
// A read fetches only the parts that it needs, and writes each one as it
// arrives.
package journal

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip44"
	"github.com/gezibash/arc/delivery/keys"
	"github.com/gezibash/arc/delivery/node"
	"github.com/gezibash/arc/delivery/store"
	"github.com/gezibash/arc/delivery/transport"
)

// The kinds and limits of the journal.
const (
	HeadKind nostr.Kind = 30078
	PartKind nostr.Kind = 3275

	// PartBytes caps the text of one part. A sealed part of this size stays
	// under 64 KiB, which is the event limit of common relays.
	PartBytes = 32 * 1024
	// MaxParts caps the parts of one page, so the head stays under the same
	// limit. A page therefore holds at most 8 MiB of text.
	MaxParts = 256

	dPrefix = "arc-journal:"
	// batch is how many parts a read asks for at a time.
	batch = 8
)

// Errors of the journal.
var (
	ErrAddress  = errors.New("journal: an address is project/notebook/page, each part [a-z0-9][a-z0-9-_.]*")
	ErrNotFound = errors.New("journal: no page at that address")
	ErrTooLarge = fmt.Errorf("journal: a page holds at most %d parts of %d bytes", MaxParts, PartBytes)
)

var segment = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

// PartRef names one part of a page, in order.
type PartRef struct {
	ID    string `json:"id"`
	Bytes int    `json:"bytes"`
	// Lines counts the newlines in the part.
	Lines int `json:"lines"`
}

// Page is one head, opened.
type Page struct {
	Address string    `json:"address"`
	Title   string    `json:"title"`
	Updated time.Time `json:"updated"`
	// Base names the write that began this text. An append keeps it, and a
	// write replaces it, so a reader can tell new text from rewritten text.
	Base  string    `json:"base"`
	Parts []PartRef `json:"parts"`

	head nostr.Event
}

// Bytes is the size of the page text.
func (p Page) Bytes() int {
	total := 0
	for _, part := range p.Parts {
		total += part.Bytes
	}
	return total
}

// Journal is the notebook of one owner.
type Journal struct {
	key        keys.Key
	idKey      []byte
	conv       [32]byte
	node       *node.Node
	transports []transport.Transport
	now        func() time.Time
	// unsent names the transports that failed a send since the last call to
	// Unsent.
	unsent map[string]error
}

// New opens the journal of a key on a node. The transports are where the
// journal sends each event, and where it looks for events that the store
// lacks.
func New(k keys.Key, n *node.Node, transports []transport.Transport) (*Journal, error) {
	idKey, err := hkdf.Key(sha256.New, k.Secret[:], nil, "arc-journal-v1 id", 32)
	if err != nil {
		return nil, err
	}
	conv, err := nip44.GenerateConversationKey(k.Public, k.Secret)
	if err != nil {
		return nil, err
	}
	return &Journal{key: k, idKey: idKey, conv: conv, node: n, transports: transports, now: time.Now}, nil
}

// Filter matches every head and part of this journal. Sync uses it.
func (j *Journal) Filter() nostr.Filter {
	return nostr.Filter{Kinds: []nostr.Kind{HeadKind, PartKind}, Authors: []nostr.PubKey{j.key.Public}}
}

// tag is the d tag of the head of an address.
func (j *Journal) tag(address string) (string, error) {
	parts := strings.Split(address, "/")
	if len(parts) != 3 {
		return "", ErrAddress
	}
	for _, part := range parts {
		if !segment.MatchString(part) {
			return "", ErrAddress
		}
	}

	mac := hmac.New(sha256.New, j.idKey)
	mac.Write([]byte("page\x00" + address))
	return dPrefix + hex.EncodeToString(mac.Sum(nil)), nil
}

func (j *Journal) coordinate(tag string) string {
	return fmt.Sprintf("%d:%s:%s", HeadKind, j.key.Public.Hex(), tag)
}

// Write replaces the text of a page with the body, and creates the page when
// it is not there. It sends each part as soon as it is full, and the head last,
// so a reader never sees a head whose parts are not sent.
func (j *Journal) Write(ctx context.Context, address, title string, body io.Reader) (Page, error) {
	tag, err := j.tag(address)
	if err != nil {
		return Page{}, err
	}

	previous, _ := j.page(ctx, address, tag)
	if title == "" && previous != nil {
		title = previous.Title
	}

	parts, err := j.writeParts(ctx, tag, body, 0)
	if err != nil {
		return Page{}, err
	}

	page := Page{Address: address, Title: title, Base: randomHex(), Parts: parts}
	return j.writeHead(ctx, tag, page, previous)
}

// Append adds text to the end of a page, and creates the page when it is not
// there. When the last part has room, the text joins it, so a page that grows
// by small notes keeps few parts.
func (j *Journal) Append(ctx context.Context, address string, text io.Reader) (Page, error) {
	tag, err := j.tag(address)
	if err != nil {
		return Page{}, err
	}

	previous, err := j.page(ctx, address, tag)
	if errors.Is(err, ErrNotFound) {
		return j.Write(ctx, address, "", text)
	}
	if err != nil {
		return Page{}, err
	}

	page := *previous
	page.Parts = append([]PartRef(nil), previous.Parts...)

	input := text
	if n := len(page.Parts); n > 0 && page.Parts[n-1].Bytes < PartBytes {
		// The last part joins the front of the new text.
		last, err := j.partText(ctx, tag, page.Parts[n-1])
		if err != nil {
			return Page{}, err
		}
		page.Parts = page.Parts[:n-1]
		input = io.MultiReader(strings.NewReader(last), text)
	}

	added, err := j.writeParts(ctx, tag, input, len(page.Parts))
	if err != nil {
		return Page{}, err
	}
	page.Parts = append(page.Parts, added...)
	return j.writeHead(ctx, tag, page, previous)
}

// writeParts splits the text into parts at line ends, and sends each part.
// A line longer than a part is split inside the line.
func (j *Journal) writeParts(ctx context.Context, tag string, body io.Reader, already int) ([]PartRef, error) {
	reader := bufio.NewReaderSize(body, PartBytes)
	var parts []PartRef
	var current strings.Builder

	flush := func() error {
		if current.Len() == 0 {
			return nil
		}
		if already+len(parts)+1 > MaxParts {
			return ErrTooLarge
		}
		ref, err := j.sendPart(ctx, tag, current.String())
		if err != nil {
			return err
		}
		parts = append(parts, ref)
		current.Reset()
		return nil
	}

	for {
		line, err := reader.ReadString('\n')
		for len(line) > 0 {
			room := PartBytes - current.Len()
			if len(line) <= room {
				current.WriteString(line)
				line = ""
				break
			}
			if current.Len() > 0 && len(line) <= PartBytes {
				// The whole line fits in a new part: start one.
				if ferr := flush(); ferr != nil {
					return nil, ferr
				}
				continue
			}
			current.WriteString(line[:room])
			line = line[room:]
			if ferr := flush(); ferr != nil {
				return nil, ferr
			}
		}

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}

	if err := flush(); err != nil {
		return nil, err
	}
	return parts, nil
}

func (j *Journal) sendPart(ctx context.Context, tag, text string) (PartRef, error) {
	sealed, err := nip44.Encrypt(text, j.conv)
	if err != nil {
		return PartRef{}, err
	}

	event := nostr.Event{
		Kind:      PartKind,
		CreatedAt: nostr.Timestamp(j.now().Unix()),
		Tags:      nostr.Tags{{"a", j.coordinate(tag)}},
		Content:   sealed,
	}
	if err := event.Sign(j.key.Secret); err != nil {
		return PartRef{}, err
	}
	if err := j.publish(ctx, event); err != nil {
		return PartRef{}, err
	}
	return PartRef{ID: event.ID.Hex(), Bytes: len(text), Lines: strings.Count(text, "\n")}, nil
}

func (j *Journal) writeHead(ctx context.Context, tag string, page Page, previous *Page) (Page, error) {
	page.Updated = j.now().UTC().Truncate(time.Second)

	body, err := json.Marshal(page)
	if err != nil {
		return Page{}, err
	}
	sealed, err := nip44.Encrypt(string(body), j.conv)
	if err != nil {
		return Page{}, err
	}

	// A head replaces an older head only when it is newer. Two writes in one
	// second would tie, so the new head always moves one second on.
	at := nostr.Timestamp(j.now().Unix())
	if previous != nil && at <= previous.head.CreatedAt {
		at = previous.head.CreatedAt + 1
	}

	event := nostr.Event{
		Kind:      HeadKind,
		CreatedAt: at,
		Tags:      nostr.Tags{{"d", tag}},
		Content:   sealed,
	}
	if err := event.Sign(j.key.Secret); err != nil {
		return Page{}, err
	}
	if err := j.publish(ctx, event); err != nil {
		return Page{}, err
	}

	page.head = event
	return page, nil
}

// publish keeps an event and sends it. A transport that fails does not fail
// the write: the event is in the store, and a later sync sends it. Unsent
// reports the failure.
func (j *Journal) publish(ctx context.Context, event nostr.Event) error {
	result, sent, err := j.node.Publish(ctx, event, j.transports)
	if err != nil {
		return err
	}
	if result.Outcome == store.Refused {
		return fmt.Errorf("journal: the store refused an event of this journal: %s", result.Reason)
	}
	for _, one := range sent {
		if one.Err != nil {
			if j.unsent == nil {
				j.unsent = map[string]error{}
			}
			j.unsent[one.Transport] = one.Err
		}
	}
	return nil
}

// Unsent returns each transport that failed a send since the last call, with
// its last error. The events are in the store, so a sync sends them later.
func (j *Journal) Unsent() map[string]error {
	out := j.unsent
	j.unsent = nil
	return out
}

// Page returns the newest head of an address.
func (j *Journal) Page(ctx context.Context, address string) (Page, error) {
	tag, err := j.tag(address)
	if err != nil {
		return Page{}, err
	}
	page, err := j.page(ctx, address, tag)
	if err != nil {
		return Page{}, err
	}
	return *page, nil
}

func (j *Journal) headFilter(tag string) nostr.Filter {
	return nostr.Filter{
		Kinds:   []nostr.Kind{HeadKind},
		Authors: []nostr.PubKey{j.key.Public},
		Tags:    nostr.TagMap{"d": []string{tag}},
	}
}

// page reads the newest head from the store. When the store has none, it
// asks the transports first.
func (j *Journal) page(ctx context.Context, address, tag string) (*Page, error) {
	heads := j.node.Store.Query(j.headFilter(tag))
	if len(heads) == 0 && len(j.transports) > 0 {
		j.node.Pull(ctx, j.headFilter(tag), j.transports)
		heads = j.node.Store.Query(j.headFilter(tag))
	}
	if len(heads) == 0 {
		return nil, ErrNotFound
	}

	page, err := j.open(heads[0])
	if err != nil {
		return nil, err
	}
	// A head is found by an HMAC of its address, so it must name that address.
	if page.Address != address {
		return nil, fmt.Errorf("journal: the head for %s names another address", address)
	}
	return &page, nil
}

func (j *Journal) open(head nostr.Event) (Page, error) {
	body, err := nip44.Decrypt(head.Content, j.conv)
	if err != nil {
		return Page{}, fmt.Errorf("journal: a head does not open with this key: %w", err)
	}

	var page Page
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		return Page{}, fmt.Errorf("journal: a head holds no page: %w", err)
	}
	page.head = head
	return page, nil
}

// List returns every page, by address. It opens heads only, and fetches no
// part. It counts the heads that did not open.
func (j *Journal) List() ([]Page, int) {
	heads := j.node.Store.Query(nostr.Filter{Kinds: []nostr.Kind{HeadKind}, Authors: []nostr.PubKey{j.key.Public}})

	var pages []Page
	unreadable := 0
	for _, head := range heads {
		if !strings.HasPrefix(head.Tags.GetD(), dPrefix) {
			continue
		}
		page, err := j.open(head)
		if err != nil {
			unreadable++
			continue
		}
		pages = append(pages, page)
	}

	sort.Slice(pages, func(a, b int) bool { return pages[a].Address < pages[b].Address })
	return pages, unreadable
}

// Lines is a range of lines, counted from 1. The zero value is the whole page.
type Lines struct {
	From, To int
}

// ParseLines reads "a:b", "a:" or ":b".
func ParseLines(value string) (Lines, error) {
	if value == "" {
		return Lines{}, nil
	}
	from, to, found := strings.Cut(value, ":")
	if !found {
		return Lines{}, errors.New("journal: a range of lines is a:b")
	}

	var r Lines
	if from != "" {
		if _, err := fmt.Sscan(from, &r.From); err != nil || r.From < 1 {
			return Lines{}, errors.New("journal: a range starts at line 1 or later")
		}
	}
	if to != "" {
		if _, err := fmt.Sscan(to, &r.To); err != nil || r.To < 1 {
			return Lines{}, errors.New("journal: a range ends at line 1 or later")
		}
	}
	if r.From != 0 && r.To != 0 && r.To < r.From {
		return Lines{}, errors.New("journal: a range ends before it starts")
	}
	return r, nil
}

func (r Lines) first() int {
	if r.From == 0 {
		return 1
	}
	return r.From
}

func (r Lines) last() int {
	if r.To == 0 {
		return int(^uint(0) >> 1)
	}
	return r.To
}

// Read writes the lines of a page to w. It fetches only the parts that hold
// those lines, a few at a time, and writes each part as it arrives. If a part
// is missing, Read stops with an error after the text before it.
func (j *Journal) Read(ctx context.Context, address string, lines Lines, w io.Writer) (Page, error) {
	page, err := j.Page(ctx, address)
	if err != nil {
		return Page{}, err
	}

	selected, start := selectParts(page.Parts, lines)
	out := &lineWriter{w: w, line: start, from: lines.first(), to: lines.last()}

	for i := 0; i < len(selected); i += batch {
		end := min(i+batch, len(selected))
		if err := j.streamParts(ctx, page.head.Tags.GetD(), selected[i:end], out); err != nil {
			return page, err
		}
		if out.done() {
			break
		}
	}
	return page, nil
}

// selectParts returns the parts that can hold a line in the range, and the
// number of the first line of the first part.
func selectParts(parts []PartRef, lines Lines) ([]PartRef, int) {
	from, to := lines.first(), lines.last()
	var selected []PartRef
	first := 0

	start := 1
	for _, part := range parts {
		next := start + part.Lines
		// The part covers lines start to next, or to next-1 when it ends
		// with a newline. Taking next as the end is safe: the writer trims.
		if start <= to && next >= from {
			if selected == nil {
				first = start
			}
			selected = append(selected, part)
		}
		start = next
	}
	return selected, first
}

func (j *Journal) streamParts(ctx context.Context, tag string, refs []PartRef, out io.Writer) error {
	ids := make([]nostr.ID, 0, len(refs))
	for _, ref := range refs {
		id, err := nostr.IDFromHex(ref.ID)
		if err != nil {
			return fmt.Errorf("journal: the head names a part with a bad id: %s", ref.ID)
		}
		ids = append(ids, id)
	}

	found, err := j.node.Obtain(ctx, ids, j.transports)
	if err != nil {
		return err
	}

	for i, id := range ids {
		event, ok := found[id]
		if !ok {
			return fmt.Errorf("journal: part %s is missing; sync, then read again", refs[i].ID)
		}
		text, err := j.openPart(tag, event)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(out, text); err != nil {
			return err
		}
	}
	return nil
}

func (j *Journal) partText(ctx context.Context, tag string, ref PartRef) (string, error) {
	var b strings.Builder
	if err := j.streamParts(ctx, tag, []PartRef{ref}, &b); err != nil {
		return "", err
	}
	return b.String(), nil
}

// openPart checks that a part belongs to this page, and opens it.
func (j *Journal) openPart(tag string, event nostr.Event) (string, error) {
	if event.Kind != PartKind || event.PubKey != j.key.Public {
		return "", fmt.Errorf("journal: event %s is not a part of this journal", event.ID.Hex())
	}
	if event.Tags.FindWithValue("a", j.coordinate(tag)) == nil {
		return "", fmt.Errorf("journal: part %s belongs to another page", event.ID.Hex())
	}
	text, err := nip44.Decrypt(event.Content, j.conv)
	if err != nil {
		return "", fmt.Errorf("journal: part %s does not open with this key: %w", event.ID.Hex(), err)
	}
	return text, nil
}

// Tail writes the page, then each piece of text that is appended to it, until
// the context ends. When someone rewrites the page, Tail says so and writes
// the new text from its start.
func (j *Journal) Tail(ctx context.Context, address string, live transport.Live, w io.Writer) error {
	tag, err := j.tag(address)
	if err != nil {
		return err
	}

	heads, err := j.node.Watch(ctx, j.headFilter(tag), live)
	if err != nil {
		return err
	}

	base, written := "", 0
	for head := range heads {
		page, err := j.open(head)
		if err != nil || page.Address != address {
			continue
		}
		if page.Base != base {
			if base != "" {
				fmt.Fprintf(w, "\n--- %s was rewritten ---\n", address)
			}
			base, written = page.Base, 0
		}

		parts, start := partsFrom(page.Parts, written)
		skip := &skipWriter{w: w, skip: written - start}
		if err := j.streamParts(ctx, tag, parts, skip); err != nil {
			fmt.Fprintf(w, "\n--- %v ---\n", err)
			continue
		}
		written = page.Bytes()
	}
	return ctx.Err()
}

// partsFrom returns the parts that hold text at or after an offset, and the
// offset at which the first of them starts. The caller skips the bytes
// between the two.
func partsFrom(parts []PartRef, offset int) ([]PartRef, int) {
	at := 0
	for i, part := range parts {
		if at+part.Bytes > offset {
			return parts[i:], at
		}
		at += part.Bytes
	}
	return nil, at
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// lineWriter passes on the lines of a range, and drops the rest.
type lineWriter struct {
	w        io.Writer
	line     int
	from, to int
}

func (l *lineWriter) done() bool { return l.line > l.to }

func (l *lineWriter) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 && !l.done() {
		i := bytes.IndexByte(p, '\n')
		chunk := p
		if i >= 0 {
			chunk = p[:i+1]
		}
		if l.line >= l.from {
			if _, err := l.w.Write(chunk); err != nil {
				return 0, err
			}
		}
		if i >= 0 {
			l.line++
		}
		p = p[len(chunk):]
	}
	return total, nil
}

// skipWriter drops the first bytes it receives, then passes on the rest.
type skipWriter struct {
	w    io.Writer
	skip int
}

func (s *skipWriter) Write(p []byte) (int, error) {
	total := len(p)
	if s.skip >= len(p) {
		s.skip -= len(p)
		return total, nil
	}
	if _, err := s.w.Write(p[s.skip:]); err != nil {
		return 0, err
	}
	s.skip = 0
	return total, nil
}
