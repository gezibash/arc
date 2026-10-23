package iface

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/sdk/terminal"
)

// A page of a kind with notebook_entry has a head, which is written once,
// and entries, which are never revised. See docs/interface/SPEC.md, section
// 3.9.3.

// reservedPages are the words that never name a page. arc resolves next
// and today, and keeps at for a time in an address.
var reservedPages = []string{"next", "today", "at"}

// entryID is the id of an entry: 16 hex digits.
var entryID = regexp.MustCompile(`^[0-9a-f]{16}$`)

// pageEntry is one entry of a page.
type pageEntry struct {
	e *entry
	// d is the d tag of the event inside: <address>/<id>.
	d        string
	page     string
	corrects string
	created  nostr.Timestamp
	// wrap is the ID of the draft, which a delete names.
	wrap nostr.ID
}

// pageEntries opens each entry of the notebook kind once, and groups the
// entries by page, oldest first. The page tag is sealed, so the store cannot
// select the entries of one page.
func (r *run) pageEntries(kind string) (map[string][]pageEntry, error) {
	name := r.kindOf(kind).NotebookEntry
	if name == "" {
		return nil, nil
	}
	number := nostr.Kind(r.kindOf(name).Kind)
	wraps, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.Kind}, nostr.TagMap{"k": {strconv.Itoa(int(number))}}))
	if err != nil {
		return nil, err
	}
	out := map[string][]pageEntry{}
	for _, wrap := range wraps {
		opened, err := r.opener().Open(r.ctx, wrap)
		if err != nil {
			fmt.Fprintf(r.stdio.Err, "left out %s: %s\n", wrap.ID.Hex()[:8], terminal.EscapeLine(err.Error())) //arc:raw-ok an event ID in hex
			continue
		}
		if opened.Deleted || opened.Event.Kind != number {
			continue
		}
		p := pageEntry{d: opened.Event.Tags.GetD(), created: opened.Event.CreatedAt, wrap: wrap.ID}
		if tag := opened.Event.Tags.Find("page"); tag != nil {
			p.page = tag[1]
		}
		if p.page == "" || !strings.HasPrefix(p.d, p.page+"/") {
			continue
		}
		if tag := opened.Event.Tags.Find("corrects"); tag != nil {
			p.corrects = tag[1]
		}
		p.e = entryOf(opened, name)
		p.e.rec["device"] = opened.Wrap.PubKey.Hex()
		out[p.page] = append(out[p.page], p)
	}
	for _, entries := range out {
		slices.SortFunc(entries, func(a, b pageEntry) int {
			if a.created != b.created {
				return int(a.created) - int(b.created)
			}
			return strings.Compare(a.d, b.d)
		})
	}
	return out, nil
}

// pageText is the text of a page: the head, then each entry, oldest first.
// The newest fix of an entry takes its place, and the updated_at field shows
// the time of the newest entry.
func (r *run) pageText(head *entry, address string, entries []pageEntry) (string, error) {
	text := ""
	if head != nil {
		var err error
		if text, err = r.joined(head); err != nil {
			return "", err
		}
	}
	if len(entries) == 0 {
		return text, nil
	}
	fixes := map[string]pageEntry{}
	for _, p := range entries {
		if p.corrects != "" {
			fixes[p.corrects] = p
		}
	}
	if fix, ok := fixes[address]; ok {
		body, err := r.joined(fix.e)
		if err != nil {
			return "", err
		}
		_, old, err := frontmatter(text)
		if err != nil {
			old = text
		}
		text = text[:len(text)-len(old)] + body
	}
	// The time of an entry is in seconds, and the time of the head is not:
	// updated_at is never earlier than the head.
	newest := time.Unix(int64(entries[len(entries)-1].created), 0).UTC()
	if fields, _, err := frontmatter(text); err == nil {
		if head, err := time.Parse(time.RFC3339Nano, str(fields["updated_at"])); err != nil || newest.After(head) {
			text = setField(text, "updated_at", newest.Format(time.RFC3339Nano))
		}
	}
	var b strings.Builder
	b.WriteString(text)
	for _, p := range entries {
		if p.corrects != "" {
			continue
		}
		if fix, ok := fixes[p.d]; ok {
			p = fix
		}
		body, err := r.joined(p.e)
		if err != nil {
			return "", err
		}
		if b.Len() > 0 && !strings.HasSuffix(b.String(), "\n") {
			b.WriteString("\n")
		}
		b.WriteString(body)
	}
	return b.String(), nil
}

// setField sets one field of the front matter, when the text has front
// matter and the field.
func setField(text, key, value string) string {
	fields, body, err := frontmatter(text)
	if err != nil || fields[key] == nil {
		return text
	}
	header := text[:len(text)-len(body)]
	lines := strings.SplitAfter(header, "\n")
	for i, line := range lines {
		name, _, _ := strings.Cut(line, ":")
		if i > 0 && name == key {
			end := "\n"
			if strings.HasSuffix(line, "\r\n") {
				end = "\r\n"
			}
			lines[i] = key + ": " + value + end
			break
		}
	}
	return strings.Join(lines, "") + body
}

// compose puts the whole text of each page in its record, so that sections,
// the index and the output read the head and the entries.
func (r *run) compose(pages []notebookPage) error {
	for i := range pages {
		p := &pages[i]
		if len(p.entries) == 0 {
			// The head alone keeps its parts, so a range of lines reads
			// only the parts that hold it.
			continue
		}
		text, err := r.pageText(p.entry, p.address, p.entries)
		if err != nil {
			return err
		}
		p.entry.rec["content"] = text
		p.entry.parts, p.entry.lines, p.entry.wrap = nil, nil, nil
	}
	return nil
}

// entryList is what the record of a page says of its entries.
func entryList(entries []pageEntry) []any {
	out := []any{}
	for _, p := range entries {
		item := map[string]any{"d": p.d, "created": int64(p.created), "device": p.e.rec["device"]}
		if p.corrects != "" {
			item["corrects"] = p.corrects
		}
		out = append(out, item)
	}
	return out
}

// scopedWith is a copy of the run whose templates know the values. A text
// that a citizen gives goes through a value, so it is never a template.
func (r *run) scopedWith(values Values) *run {
	scoped := *r
	scoped.values = maps.Clone(r.values)
	maps.Copy(scoped.values, values)
	return &scoped
}

// headD is the d tag of the head of a page, from the d template of the
// action.
func (r *run) headD(template, address string) (string, error) {
	return r.scopedWith(Values{"address": address}).template(template)
}

// head reads the head of a page, or nil.
func (r *run) head(kind, d string) (*entry, error) {
	entries, err := r.query(&Query{Kinds: []string{kind}, Authors: "me", D: d, Limit: "1"})
	if err != nil || len(entries) == 0 {
		return nil, err
	}
	return entries[0], nil
}

// today is the UTC date of the clock, as a page title.
func (r *run) today() string { return r.env.Now().UTC().Format("2006-01-02") }

// resolve gives the page that an address names. next is the highest page
// number of the notebook plus one, and today is the page with the lowest
// number whose title is the date. For today, found says whether the page
// exists.
func (r *run) resolve(kind, address string, allow ...string) (string, bool, error) {
	slash := strings.LastIndex(address, "/")
	book, word := address[:slash], address[slash+1:]
	if !slices.Contains(reservedPages, word) {
		return address, true, nil
	}
	if !slices.Contains(allow, word) {
		return "", false, fmt.Errorf("%s names no page here: arc keeps the words %s", terminal.EscapeLine(address), strings.Join(reservedPages, ", "))
	}
	pages, err := r.notebookPages(kind)
	if err != nil {
		return "", false, err
	}
	highest := int64(0)
	var dated []notebookPage
	for _, p := range pages {
		if p.book != book || p.page == 0 {
			continue
		}
		highest = max(highest, p.page)
		if p.title == r.today() {
			dated = append(dated, p)
		}
	}
	next := book + "/" + strconv.FormatInt(highest+1, 10)
	if word == "next" {
		return next, false, nil
	}
	if len(dated) == 0 {
		return next, false, nil
	}
	slices.SortFunc(dated, func(a, b notebookPage) int { return int(a.page - b.page) })
	return dated[0].address, true, nil
}

// addEntry adds one entry to a page. Its time is later than each other
// entry of the page, so two entries of one second keep their order.
func (r *run) addEntry(kind, address, text, corrects string, entries []pageEntry) error {
	name := r.kindOf(kind).NotebookEntry
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	d := address + "/" + hex.EncodeToString(id)
	key, err := r.keyed(name, d)
	if err != nil {
		return err
	}
	at := nostr.Timestamp(r.env.Now().Unix())
	for _, p := range entries {
		if p.created >= at {
			at = p.created + 1
		}
	}
	scoped := r.scopedWith(Values{"entry_text": text, "entry_d": d, "entry_page": address, "entry_corrects": corrects})
	_, err = scoped.publishSealed(&Publish{
		Kind: name, D: key, Revise: "entry", at: at,
		Content: Content{Text: "{{entry_text}}"},
		Tags:    [][]string{{"d", "{{entry_d}}"}, {"page", "{{entry_page}}"}, {"corrects", "{{entry_corrects}}"}},
	})
	return err
}

// writePage makes the head of a new page: its front matter and its first
// text. It fills in the front matter that the text does not give.
func (r *run) writePage(n *Notebook, dTemplate string) (string, error) {
	address, _, err := r.resolve(n.Kind, n.Address, "next", "today")
	if err != nil {
		return "", err
	}
	slash := strings.LastIndex(address, "/")
	book, number := address[:slash], address[slash+1:]
	if _, err := ordinal(number); err != nil {
		return "", fmt.Errorf("a new page has a number, not %q", number)
	}
	if strings.HasSuffix(n.Address, "/today") && n.Title == "" {
		n.Title = r.today()
	}
	d, err := r.headD(dTemplate, address)
	if err != nil {
		return "", err
	}
	if old, err := r.current(d); err != nil {
		return "", err
	} else if old != nil && !old.Deleted {
		return "", fmt.Errorf("the page %s exists: a page keeps each entry, so journal append adds one, and journal fix corrects one", terminal.EscapeLine(address))
	}
	fields, body, err := frontmatter(n.Text)
	if err != nil {
		return "", err
	}
	title := str(fields["title"])
	switch {
	case n.Title != "" && title != "" && title != n.Title:
		return "", errors.New("frontmatter: title conflicts with --title")
	case title == "" && n.Title != "":
		title = n.Title
	case title == "":
		title = heading(body)
	}
	if strings.TrimSpace(body) == "" && title == "" {
		return "", errors.New("no text: give the page on the standard input, or give a --title")
	}
	if title == "" {
		title = r.today()
	}
	for key, want := range map[string]string{"notebook": book, "page": number} {
		if given := str(fields[key]); given != "" && given != want {
			return "", fmt.Errorf("frontmatter: %s %q does not match the address %s", key, given, terminal.EscapeLine(address))
		}
	}
	header := "---\n---\n"
	if len(fields) > 0 {
		header = n.Text[:len(n.Text)-len(body)]
	}
	for _, key := range []string{"title", "notebook", "page"} {
		value := quoted(map[string]string{"title": title, "notebook": book, "page": number}[key])
		if given, ok := fields[key]; ok {
			// A field that is given and empty takes the value in its place.
			if strings.TrimSpace(str(given)) == "" {
				header = setField(header, key, value)
			}
		} else {
			end := strings.LastIndex(header, "---")
			header = header[:end] + key + ": " + value + "\n" + header[end:]
		}
	}
	scoped := r.scopedWith(Values{"head_text": header + body, "head_address": address, "head_title": title})
	if _, err := scoped.publishSealed(&Publish{
		Kind: n.Kind, D: d, Revise: "replace", FrontmatterAddress: "{{head_address}}",
		Content: Content{Text: "{{head_text}}"},
		Tags:    [][]string{{"d", "{{head_address}}"}, {"title", "{{head_title}}"}},
	}); err != nil {
		return "", err
	}
	return address, nil
}

// heading is the text of the first "# " heading.
func heading(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if title, ok := strings.CutPrefix(strings.TrimRight(line, "\r"), "# "); ok {
			return strings.TrimSpace(title)
		}
	}
	return ""
}

// quoted writes a value of the front matter, in quotes when the plain form
// would read as something else.
func quoted(value string) string {
	if value != strings.TrimSpace(value) || strings.ContainsAny(value[:min(1, len(value))], "\"'[{") || value == "|" || value == ">" {
		return strconv.Quote(value)
	}
	return value
}

// appendEntry adds an entry to a page that exists, or to today's page,
// which it makes when it does not exist.
func (r *run) appendEntry(n *Notebook, dTemplate string) (string, error) {
	if strings.TrimSpace(n.Text) == "" {
		return "", errors.New("no text to add")
	}
	address, found, err := r.resolve(n.Kind, n.Address, "today")
	if err != nil {
		return "", err
	}
	if !found {
		made := *n
		made.Address, made.Text, made.Title = address, "", r.today()
		if _, err := r.writePage(&made, dTemplate); err != nil {
			return "", err
		}
	}
	entries, err := r.pageEntries(n.Kind)
	if err != nil {
		return "", err
	}
	if found {
		if err := r.mustExist(n.Kind, dTemplate, address, entries[address]); err != nil {
			return "", err
		}
	}
	return address, r.addEntry(n.Kind, address, n.Text, "", entries[address])
}

// mustExist fails when a page has no head and no entry.
func (r *run) mustExist(kind, dTemplate, address string, entries []pageEntry) error {
	if len(entries) > 0 {
		return nil
	}
	d, err := r.headD(dTemplate, address)
	if err != nil {
		return err
	}
	head, err := r.head(kind, d)
	if err != nil {
		return err
	}
	if head == nil {
		return noSuchPage(address)
	}
	return nil
}

func noSuchPage(address string) error {
	return fmt.Errorf("no such page %s", terminal.EscapeLine(address))
}

// fixEntry adds an entry that corrects an entry, or the head when the
// address names a page.
func (r *run) fixEntry(n *Notebook, dTemplate string) (string, error) {
	if strings.TrimSpace(n.Text) == "" {
		return "", errors.New("no text for the fix")
	}
	address, target := n.Address, n.Address
	if parts := strings.Split(n.Address, "/"); len(parts) == 4 {
		if !entryID.MatchString(parts[3]) {
			return "", fmt.Errorf("invalid entry %q", parts[3])
		}
		address = strings.Join(parts[:3], "/")
	}
	if _, _, err := r.notebookAddress(address); err != nil {
		return "", err
	}
	if _, _, err := r.resolve(n.Kind, address); err != nil {
		return "", err
	}
	entries, err := r.pageEntries(n.Kind)
	if err != nil {
		return "", err
	}
	if target == address {
		if err := r.mustExist(n.Kind, dTemplate, address, nil); err != nil {
			return "", err
		}
	} else {
		i := slices.IndexFunc(entries[address], func(p pageEntry) bool { return p.d == target })
		if i < 0 {
			return "", fmt.Errorf("no such entry %s", terminal.EscapeLine(target))
		}
		if first := entries[address][i].corrects; first != "" {
			target = first
		}
	}
	return address, r.addEntry(n.Kind, address, n.Text, target, entries[address])
}

// history lists the checkpoints of the head, then each entry and fix.
func (r *run) history(n *Notebook, dTemplate string) ([]*entry, error) {
	address, found, err := r.resolve(n.Kind, n.Address, "today")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, noSuchPage(address)
	}
	d, err := r.headD(dTemplate, address)
	if err != nil {
		return nil, err
	}
	out, err := r.scopedWith(Values{"history_d": d}).query(&Query{Kinds: []string{n.Kind}, Authors: "me", D: "{{history_d}}", History: true})
	if err != nil {
		return nil, err
	}
	entries, err := r.pageEntries(n.Kind)
	if err != nil {
		return nil, err
	}
	for _, p := range entries[address] {
		out = append(out, p.e)
	}
	if len(out) == 0 {
		return nil, noSuchPage(address)
	}
	return out, nil
}

// deletePage deletes the head of a page, as a delete action does, and each
// entry and its parts with one request.
func (r *run) deletePage(n *Notebook, dTemplate string) error {
	// A delete names its page: it does not resolve today.
	if _, _, err := r.resolve(n.Kind, n.Address); err != nil {
		return err
	}
	entries, err := r.pageEntries(n.Kind)
	if err != nil {
		return err
	}
	list := entries[n.Address]
	if len(list) > 0 {
		if r.flags.DryRun {
			return r.dryRun(map[string]any{"delete": n.Address, "entries": len(list)})
		}
		me := r.env.Me()
		tags := nostr.Tags{}
		for _, p := range list {
			tags = append(tags, nostr.Tag{"a", draft.Coordinate(me, p.e.d)}, nostr.Tag{"e", p.wrap.Hex()})
			for _, id := range p.e.parts {
				tags = append(tags, nostr.Tag{"e", id})
			}
		}
		for _, kind := range []nostr.Kind{draft.Kind, draft.PartKind} {
			tags = append(tags, nostr.Tag{"k", strconv.Itoa(int(kind))})
		}
		k, err := r.env.Sealer(r.ctx, r.in.Manifest.ID)
		if err != nil {
			return err
		}
		at := nostr.Now()
		for _, p := range list {
			if p.created >= at {
				at = p.created + 1
			}
		}
		request, err := k.Deletion(r.ctx, tags, at)
		if err != nil {
			return err
		}
		if err := r.env.Publish(r.ctx, []nostr.Event{request}, nil); err != nil {
			return err
		}
	}
	scoped := r.scopedWith(Values{"address": n.Address})
	if _, err := scoped.remove(&Delete{Kind: n.Kind, D: dTemplate}); err != nil && (len(list) == 0 || err.Error() != "there is nothing to delete") {
		return err
	}
	return nil
}

// notebookWrite runs the operations of a kind that keeps each entry.
func (r *run) notebookWrite(n *Notebook, dTemplate string) ([]*entry, error) {
	if n.Op != "fix" {
		address, _, err := r.notebookAddress(n.Address)
		if err != nil {
			return nil, err
		}
		n.Address = address
	}
	var address string
	var err error
	switch n.Op {
	case "write":
		address, err = r.writePage(n, dTemplate)
	case "append":
		address, err = r.appendEntry(n, dTemplate)
	case "fix":
		address, err = r.fixEntry(n, dTemplate)
	case "history":
		return r.history(n, dTemplate)
	case "delete":
		return nil, r.deletePage(n, dTemplate)
	}
	if err != nil || r.flags.DryRun || r.command.Output.Format == "" && !r.flags.JSON {
		return nil, err
	}
	return []*entry{{rec: Record{"kind": n.Kind, "tags": map[string]any{"d": address}, "content": address}}}, nil
}
