package iface

import (
	"cmp"
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

// maxPage is the highest page number. A higher one would leave no next
// page.
const maxPage = 999_999_999

// pageEntry is one entry of a page.
type pageEntry struct {
	e *entry
	// d is the d tag of the event inside: <address>/<id>.
	d        string
	page     string
	head     string
	corrects string
	created  nostr.Timestamp
	// ms is the time of the entry in milliseconds, which orders a page.
	ms int64
	// wrap is the ID of the draft, which a delete names.
	wrap nostr.ID
}

// at is the time of the entry.
func (p pageEntry) at() time.Time { return time.UnixMilli(p.ms).UTC() }

// pageEntries opens each entry of the notebook kind once, and groups the
// entries by the address that they name, oldest first. The page tag is
// sealed, so the store cannot select the entries of one page.
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
		tags := opened.Event.Tags
		p := pageEntry{d: tags.GetD(), created: opened.Event.CreatedAt, ms: int64(opened.Event.CreatedAt) * 1000, wrap: wrap.ID}
		for name, value := range map[string]*string{"page": &p.page, "head": &p.head, "corrects": &p.corrects} {
			if tag := tags.Find(name); tag != nil {
				*value = tag[1]
			}
		}
		if tag := tags.Find("ms"); tag != nil {
			if ms, err := strconv.ParseInt(tag[1], 10, 64); err == nil && ms/1000 == int64(p.created) {
				p.ms = ms
			}
		}
		if p.page == "" || !strings.HasPrefix(p.d, p.page+"/") {
			continue
		}
		p.e = entryOf(opened, name)
		p.e.rec["device"] = opened.Wrap.PubKey.Hex()
		p.e.rec["at"] = p.at().Format(timeFormat)
		p.e.rec["fixes"] = ""
		if p.corrects != "" {
			p.e.rec["fixes"] = "\tfixes " + p.corrects
		}
		out[p.page] = append(out[p.page], p)
	}
	for _, entries := range out {
		sortPageEntries(entries)
	}
	return out, nil
}

func sortPageEntries(entries []pageEntry) {
	slices.SortFunc(entries, func(a, b pageEntry) int {
		if a.ms != b.ms {
			return cmp.Compare(a.ms, b.ms)
		}
		return strings.Compare(a.d, b.d)
	})
}

// attachEntries gives each page the entries that name its head. Entries
// and heads of an address that the live head does not name are a lost
// page: two devices made a page at one address offline, or one deleted it
// while the other added to it. A lost page reads at <address>-<head id>,
// on each device, with no write. A lost head with no text and the title of
// the live head, as two adds to today make, joins the live page instead.
func (r *run) attachEntries(kind string, pages []notebookPage) ([]notebookPage, error) {
	added, err := r.pageEntries(kind)
	if err != nil {
		return nil, err
	}
	me := r.env.Me()
	live := map[string]int{}
	var coordinates []string
	for i, p := range pages {
		live[p.address] = i
		if p.hid != "" {
			coordinates = append(coordinates, draft.Coordinate(me, p.entry.d))
		}
	}
	type key struct{ address, hid string }
	lostEntries := map[key][]pageEntry{}
	for address, list := range added {
		for _, e := range list {
			i, ok := live[address]
			switch {
			case ok && (e.head == "" || pages[i].hid == "" || e.head == pages[i].hid):
				pages[i].entries = append(pages[i].entries, e)
			case e.head != "":
				lostEntries[key{address, e.head}] = append(lostEntries[key{address, e.head}], e)
			}
		}
	}
	// A head is written once, so a checkpoint of another head id at the
	// address of a live head is the head of a lost page.
	lostHeads := map[key]draft.Draft{}
	lostIDs := map[key]nostr.ID{}
	if len(coordinates) > 0 {
		checkpoints, err := r.fetchSelf(r.selfFilter([]nostr.Kind{draft.CheckpointKind}, nostr.TagMap{"a": coordinates}))
		if err != nil {
			return nil, err
		}
		for _, event := range checkpoints {
			opened, err := r.opener().Open(r.ctx, event)
			if err != nil || opened.Event.Kind != nostr.Kind(r.kindOf(kind).Kind) {
				continue
			}
			address := opened.Event.Tags.GetD()
			hid := ""
			if tag := opened.Event.Tags.Find("head"); tag != nil {
				hid = tag[1]
			}
			i, ok := live[address]
			k := key{address, hid}
			if hid == "" || ok && hid == pages[i].hid {
				continue
			}
			if _, seen := lostHeads[k]; !seen {
				lostHeads[k], lostIDs[k] = opened, event.ID
			}
		}
	}
	keys := slices.Collect(maps.Keys(lostEntries))
	for k := range lostHeads {
		if _, ok := lostEntries[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b key) int { return strings.Compare(a.address+"-"+a.hid, b.address+"-"+b.hid) })
	for _, k := range keys {
		head, hasHead := lostHeads[k]
		if i, ok := live[k.address]; ok && hasHead {
			_, body, err := frontmatter(head.Event.Content)
			if err == nil && strings.TrimSpace(body) == "" && len(head.Parts()) == 0 && head.Event.Tags.Find("title") != nil && head.Event.Tags.Find("title")[1] == pages[i].title {
				pages[i].entries = append(pages[i].entries, lostEntries[k]...)
				continue
			}
		}
		address := k.address + "-" + k.hid
		var e *entry
		if hasHead {
			e = entryOf(head, kind)
		} else {
			e = &entry{rec: Record{"kind": kind, "content": "", "tags": map[string]any{"title": "entries of " + k.address}}}
		}
		tags := map[string]any{}
		if old, ok := e.rec["tags"].(map[string]any); ok {
			tags = maps.Clone(old)
		}
		tags["d"] = address
		e.rec["tags"], e.d = tags, address
		p := notebookPage{entry: e, address: address, base: k.address, hid: k.hid, lost: true, checkpoint: lostIDs[k], entries: lostEntries[k]}
		sortPageEntries(p.entries)
		pageMeta(&p)
		pages = append(pages, p)
	}
	for i := range pages {
		if !pages[i].lost {
			sortPageEntries(pages[i].entries)
			pageMeta(&pages[i])
		}
	}
	return pages, nil
}

// noteLost says, on the standard error, which pages are lost pages, and
// why. With an address, it names only the lost pages of that address.
// It gives one line for each address, which names each page by its title
// (#399, #403).
func (r *run) noteLost(pages []notebookPage, address string) {
	var bases []string
	lost := map[string][]notebookPage{}
	for _, p := range pages {
		if !p.lost || address != "" && p.base != address && p.address != address {
			continue
		}
		if lost[p.base] == nil {
			bases = append(bases, p.base)
		}
		lost[p.base] = append(lost[p.base], p)
	}
	for _, base := range bases {
		var heads, orphans []string
		for _, p := range lost[base] {
			where := fmt.Sprintf("%s reads at %s", titled(p.title), terminal.EscapeLine(p.address))
			if p.checkpoint == (nostr.ID{}) {
				orphans = append(orphans, fmt.Sprintf("entries of a page of %s that a delete on another device did not know read at %s", terminal.EscapeLine(base), terminal.EscapeLine(p.address)))
				continue
			}
			heads = append(heads, where)
		}
		if len(heads) > 0 {
			i := slices.IndexFunc(pages, func(o notebookPage) bool { return !o.lost && o.address == base })
			kept := "the page there is gone"
			if i >= 0 {
				kept = "the store keeps " + titled(pages[i].title) + " there"
			}
			n := len(heads)
			if i >= 0 {
				n++
			}
			fmt.Fprintf(r.stdio.Err, "%d pages were made at %s offline; %s; %s\n", n, terminal.EscapeLine(base), kept, strings.Join(heads, "; "))
		}
		for _, line := range orphans {
			fmt.Fprintln(r.stdio.Err, line)
		}
	}
}

// titled is a title of a page in quotes, for a line of the log.
func titled(title string) string {
	if title == "" {
		return "a page with no title"
	}
	return strconv.Quote(terminal.EscapeLine(title))
}

// findPage finds the page of an address: a live page, today's page, or a
// lost page.
func (r *run) findPage(kind, address string, allow ...string) (*notebookPage, error) {
	page, _, err := r.findPages(kind, address, allow...)
	return page, err
}

// findPages is findPage, and each page of the kind. A lost page that moved
// to its address still reads at <address>-<head id>.
func (r *run) findPages(kind, address string, allow ...string) (*notebookPage, []notebookPage, error) {
	resolved, found, err := r.resolve(kind, address, allow...)
	if err != nil {
		return nil, nil, err
	}
	if !found {
		return nil, nil, noSuchPage(address)
	}
	pages, err := r.notebookPages(kind)
	if err != nil {
		return nil, nil, err
	}
	for i := range pages {
		if pages[i].address == resolved || !pages[i].lost && pages[i].hid != "" && pages[i].address+"-"+pages[i].hid == resolved {
			r.noteLost(pages, pages[i].base)
			return &pages[i], pages, nil
		}
	}
	return nil, nil, noSuchPage(resolved)
}

// pageText is the text of a page: the head, then each entry, oldest first.
// The newest fix of an entry takes its place, and the updated_at field shows
// the time of the newest entry. A fix of the head names one address of the
// page: a lost page that moved up has two.
func (r *run) pageText(head *entry, addresses []string, entries []pageEntry) (string, error) {
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
	var fix *pageEntry
	for _, address := range addresses {
		if f, ok := fixes[address]; ok && (fix == nil || f.ms > fix.ms || f.ms == fix.ms && f.d > fix.d) {
			fix = &f
		}
	}
	if fix != nil {
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
	// updated_at is never earlier than the head.
	newest := entries[len(entries)-1].at()
	if fields, _, err := frontmatter(text); err == nil {
		if head, err := time.Parse(time.RFC3339Nano, str(fields["updated_at"])); err != nil || newest.After(head) {
			text = setField(text, "updated_at", newest.Format(timeFormat))
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
		text, err := r.pageText(p.entry, p.names(), p.entries)
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
		item := map[string]any{"d": p.d, "at": p.e.rec["at"], "device": p.e.rec["device"]}
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
// entry of the page that this device holds, by at least a millisecond, so
// entries of one device keep their order.
func (r *run) addEntry(kind string, page *notebookPage, text, corrects string) error {
	name := r.kindOf(kind).NotebookEntry
	id := make([]byte, 8)
	if _, err := rand.Read(id); err != nil {
		return err
	}
	d := page.base + "/" + hex.EncodeToString(id)
	key, err := r.keyed(name, d)
	if err != nil {
		return err
	}
	ms := r.env.Now().UnixMilli()
	for _, p := range page.entries {
		if p.ms >= ms {
			ms = p.ms + 1
		}
	}
	scoped := r.scopedWith(Values{"entry_text": text, "entry_d": d, "entry_page": page.base, "entry_head": page.hid, "entry_corrects": corrects, "entry_ms": strconv.FormatInt(ms, 10)})
	_, err = scoped.publishSealed(&Publish{
		Kind: name, D: key, Revise: "entry", at: nostr.Timestamp(ms / 1000),
		Content: Content{Text: "{{entry_text}}"},
		Tags:    [][]string{{"d", "{{entry_d}}"}, {"page", "{{entry_page}}"}, {"head", "{{entry_head}}"}, {"corrects", "{{entry_corrects}}"}, {"ms", "{{entry_ms}}"}},
	})
	return err
}

// writePage makes the head of a new page: its front matter and its first
// text, with a random head id that its entries name. It fills in the front
// matter that the text does not give.
func (r *run) writePage(n *Notebook, dTemplate string) (string, error) {
	address, _, err := r.resolve(n.Kind, n.Address, "next", "today")
	if err != nil {
		return "", err
	}
	slash := strings.LastIndex(address, "/")
	book, number := address[:slash], address[slash+1:]
	if n, err := ordinal(number); err != nil || n > maxPage {
		return "", fmt.Errorf("a new page has a number from 1 to %d, not %q", maxPage, number)
	}
	if strings.HasSuffix(n.Address, "/today") {
		// today finds its page by the date in the title.
		if n.Title != "" && n.Title != r.today() {
			return "", fmt.Errorf("the title of today's page is the date %s, not %q", r.today(), n.Title)
		}
		n.Title = r.today()
	}
	d, err := r.headD(dTemplate, address)
	if err != nil {
		return "", err
	}
	exists := fmt.Errorf("the page %s exists: a page keeps each entry, so journal append adds one, and journal fix corrects one", terminal.EscapeLine(address))
	old, err := r.current(d)
	if err != nil {
		return "", err
	}
	if old != nil && !old.Deleted {
		return "", exists
	}
	// A sync can bring the entries of a page before its head. A new head
	// would hide them, so the page exists until its head comes, or until a
	// delete of the address.
	if old == nil {
		if entries, err := r.pageEntries(n.Kind); err != nil {
			return "", err
		} else if len(entries[address]) > 0 {
			return "", exists
		}
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
		// The first line, and not the date: only today's page has the date
		// as its title, so that today names one page (#398).
		title = firstLine(body)
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
	hid := make([]byte, 8)
	if _, err := rand.Read(hid); err != nil {
		return "", err
	}
	scoped := r.scopedWith(Values{"head_text": header + body, "head_address": address, "head_title": title, "head_id": hex.EncodeToString(hid)})
	if _, err := scoped.publishSealed(&Publish{
		Kind: n.Kind, D: d, Revise: "replace", FrontmatterAddress: "{{head_address}}",
		Content: Content{Text: "{{head_text}}"},
		Tags:    [][]string{{"d", "{{head_address}}"}, {"title", "{{head_title}}"}, {"head", "{{head_id}}"}},
	}); err != nil {
		return "", err
	}
	return address, nil
}

// firstLine is the first line of the text that holds more than blanks, cut
// to 80 characters.
func firstLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			if r := []rune(line); len(r) > 80 {
				line = string(r[:80])
			}
			return line
		}
	}
	return ""
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
	page, err := r.findPage(n.Kind, address)
	if err != nil {
		return "", err
	}
	return page.address, r.addEntry(n.Kind, page, n.Text, "")
}

func noSuchPage(address string) error {
	return fmt.Errorf("no such page %s", terminal.EscapeLine(address))
}

// fixEntry adds an entry that corrects an entry, or the head when the
// address names a page.
func (r *run) fixEntry(n *Notebook) (string, error) {
	if strings.TrimSpace(n.Text) == "" {
		return "", errors.New("no text for the fix")
	}
	address, id := n.Address, ""
	if parts := strings.Split(n.Address, "/"); len(parts) == 4 {
		if !entryID.MatchString(parts[3]) {
			return "", fmt.Errorf("invalid entry %q", parts[3])
		}
		address, id = strings.Join(parts[:3], "/"), parts[3]
	}
	if _, _, err := r.notebookAddress(address); err != nil {
		return "", err
	}
	page, pages, err := r.findPages(n.Kind, address)
	if err != nil {
		return "", err
	}
	target := page.address
	if id != "" {
		target = page.base + "/" + id
		// history names an entry by the address of its page tag, so the
		// entry can be on a lost page of the address.
		i := slices.IndexFunc(page.entries, func(p pageEntry) bool { return p.d == target })
		for j := range pages {
			if i >= 0 {
				break
			}
			if pages[j].base == page.base {
				if i = slices.IndexFunc(pages[j].entries, func(p pageEntry) bool { return p.d == target }); i >= 0 {
					page = &pages[j]
				}
			}
		}
		if i < 0 {
			return "", fmt.Errorf("no such entry %s", terminal.EscapeLine(n.Address))
		}
		if first := page.entries[i].corrects; first != "" {
			target = first
		}
	}
	return page.address, r.addEntry(n.Kind, page, n.Text, target)
}

// history lists the checkpoints of the head, then each entry and fix.
func (r *run) history(n *Notebook, dTemplate string) ([]*entry, error) {
	page, err := r.findPage(n.Kind, n.Address, "today")
	if err != nil {
		return nil, err
	}
	var out []*entry
	switch {
	case page.lost && page.checkpoint != (nostr.ID{}):
		out = append(out, page.entry)
	case !page.lost:
		d, err := r.headD(dTemplate, page.base)
		if err != nil {
			return nil, err
		}
		checkpoints, err := r.scopedWith(Values{"history_d": d}).query(&Query{Kinds: []string{n.Kind}, Authors: "me", D: "{{history_d}}", History: true})
		if err != nil {
			return nil, err
		}
		for _, e := range checkpoints {
			// The checkpoint of a lost head belongs to its own page.
			if page.hid == "" || str(field(e.rec, "tags.head")) == page.hid {
				out = append(out, e)
			}
		}
	}
	for _, e := range out {
		e.rec["at"] = stamp(time.Unix(int64(number(e.rec["created"])), 0))
		e.rec["fixes"] = ""
	}
	for _, p := range page.entries {
		out = append(out, p.e)
	}
	return out, nil
}

// deletePage deletes a page. The head goes as a delete action deletes a
// draft, with the entries of the page and their parts in one more request.
// The head of a lost page is a checkpoint, so a request names it and its
// parts, and the live page at the address stays.
func (r *run) deletePage(n *Notebook, dTemplate string) error {
	// A delete names its page: it does not resolve today.
	if _, _, err := r.resolve(n.Kind, n.Address); err != nil {
		return err
	}
	pages, err := r.notebookPages(n.Kind)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(pages, func(p notebookPage) bool { return p.address == n.Address })
	if i < 0 {
		// A lost page that moved up still answers to <address>-<head id>.
		if i = slices.IndexFunc(pages, func(p notebookPage) bool {
			return !p.lost && p.hid != "" && p.address+"-"+p.hid == n.Address
		}); i >= 0 {
			n.Address = pages[i].address
		}
	}
	var page *notebookPage
	if i >= 0 {
		page = &pages[i]
	}
	// The head of a lost page is a checkpoint at the same address, so the
	// delete of the address keeps it, and the newest one moves up to it.
	var keep []string
	var up *notebookPage
	for j := range pages {
		p := &pages[j]
		if p.lost && p.base == n.Address && p.checkpoint != (nostr.ID{}) {
			keep = append(append(keep, p.checkpoint.Hex()), p.entry.parts...)
			if up == nil || number(p.entry.rec["created"]) > number(up.entry.rec["created"]) {
				up = p
			}
		}
	}
	if page != nil && (len(page.entries) > 0 || page.lost) {
		if r.flags.DryRun {
			return r.dryRun(map[string]any{"delete": n.Address, "entries": len(page.entries)})
		}
		me := r.env.Me()
		tags := nostr.Tags{}
		at := nostr.Now()
		for _, p := range page.entries {
			tags = append(tags, nostr.Tag{"a", draft.Coordinate(me, p.e.d)}, nostr.Tag{"e", p.wrap.Hex()})
			for _, id := range p.e.parts {
				tags = append(tags, nostr.Tag{"e", id})
			}
			if p.created >= at {
				at = p.created + 1
			}
		}
		if page.lost && page.checkpoint != (nostr.ID{}) {
			tags = append(tags, nostr.Tag{"e", page.checkpoint.Hex()})
			for _, id := range page.entry.parts {
				tags = append(tags, nostr.Tag{"e", id})
			}
		}
		for _, kind := range []nostr.Kind{draft.Kind, draft.CheckpointKind, draft.PartKind} {
			tags = append(tags, nostr.Tag{"k", strconv.Itoa(int(kind))})
		}
		k, err := r.env.Sealer(r.ctx, r.in.Manifest.ID)
		if err != nil {
			return err
		}
		request, err := k.Deletion(r.ctx, tags, at)
		if err != nil {
			return err
		}
		if err := r.env.Publish(r.ctx, []nostr.Event{request}, nil); err != nil {
			return err
		}
		if page.lost {
			return nil
		}
	}
	scoped := r.scopedWith(Values{"address": n.Address})
	d, err := scoped.template(dTemplate)
	if err != nil {
		return err
	}
	old, err := r.current(d)
	if err != nil {
		return err
	}
	if _, err := scoped.removeKeeping(&Delete{Kind: n.Kind, D: dTemplate}, keep); err != nil && (page == nil || len(page.entries) == 0 || err.Error() != "there is nothing to delete") {
		return err
	}
	if up == nil || r.flags.DryRun {
		return nil
	}
	gone := ""
	if page != nil && !page.lost {
		gone = page.title
	}
	return r.moveUp(n.Kind, d, old, up, gone)
}

// moveUp makes the head of a lost page the draft at its address again, as
// it was written, when the page there is deleted. Nothing then takes its
// checkpoint for one that a failed write left, and its entries, which name
// its head id, read at the address.
func (r *run) moveUp(kind, d string, old *draft.Draft, up *notebookPage, gone string) error {
	text, err := r.joined(up.entry)
	if err != nil {
		return err
	}
	// The blank draft of the delete is one second newer than the old draft
	// or the clock; this draft is newer again.
	at := nostr.Now()
	if old != nil && at <= old.Wrap.CreatedAt {
		at = old.Wrap.CreatedAt + 1
	}
	at++
	tags := [][]string{{"d", "{{up_address}}"}, {"title", "{{up_title}}"}, {"head", "{{up_head}}"}}
	scoped := r.scopedWith(Values{"up_text": text, "up_address": up.base, "up_title": up.title, "up_head": up.hid})
	if _, err := scoped.publishSealed(&Publish{Kind: kind, D: d, Revise: "entry", at: at, Content: Content{Text: "{{up_text}}"}, Tags: tags}); err != nil {
		return err
	}
	fmt.Fprintf(r.stdio.Err, "%s is deleted, so %s, made at %s offline too, moved up from %s to %s\n",
		titled(gone), titled(up.title), terminal.EscapeLine(up.base), terminal.EscapeLine(up.address), terminal.EscapeLine(up.base))
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
		address, err = r.fixEntry(n)
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

// names are the addresses of a page that a fix of its head can name: its
// address, and <address>-<head id>, which a fix made while the page was lost
// names.
func (p *notebookPage) names() []string {
	if p.lost || p.hid == "" {
		return []string{p.address}
	}
	return []string{p.address, p.address + "-" + p.hid}
}
