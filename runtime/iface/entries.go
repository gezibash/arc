package iface

import (
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/iface/args"
	"github.com/gezibash/arc/runtime/iface/manifest"
	"github.com/gezibash/arc/runtime/iface/template"
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
// why. It names the addresses of the lost pages in shown, and with an
// address, only that address. It gives one line for each address, which
// names each page by its title (#399, #403). It counts the pages of an
// address in all, the pages of the whole journal: a filter of ls can hide
// the live page.
func (r *run) noteLost(all, shown []notebookPage, address string) {
	var bases []string
	for _, p := range shown {
		if !p.lost || address != "" && p.base != address && p.address != address || slices.Contains(bases, p.base) {
			continue
		}
		bases = append(bases, p.base)
	}
	lost := map[string][]notebookPage{}
	for _, p := range all {
		if p.lost && slices.Contains(bases, p.base) {
			lost[p.base] = append(lost[p.base], p)
		}
	}
	pages := all
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
			made := fmt.Sprintf("%d pages were made", n)
			if n == 1 {
				made = "1 page was made"
			}
			fmt.Fprintf(r.stdio.Err, "%s at %s offline; %s; %s\n", made, terminal.EscapeLine(base), kept, strings.Join(heads, "; "))
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
	return `"` + terminal.EscapeLine(title) + `"`
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
		if head, err := time.Parse(time.RFC3339Nano, template.Str(fields["updated_at"])); err != nil || newest.After(head) {
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
	key, err := r.Keyed(name, d)
	if err != nil {
		return err
	}
	ms := r.env.Now().UnixMilli()
	for _, p := range page.entries {
		if p.ms >= ms {
			ms = p.ms + 1
		}
	}
	scoped := r.scopedWith(args.Values{"entry_text": text, "entry_d": d, "entry_page": page.base, "entry_head": page.hid, "entry_corrects": corrects, "entry_ms": strconv.FormatInt(ms, 10)})
	_, err = scoped.publishSealed(&manifest.Publish{
		Kind: name, D: key, Revise: "entry", At: nostr.Timestamp(ms / 1000),
		Content: manifest.Content{Text: "{{entry_text}}"},
		Tags:    [][]string{{"d", "{{entry_d}}"}, {"page", "{{entry_page}}"}, {"head", "{{entry_head}}"}, {"corrects", "{{entry_corrects}}"}, {"ms", "{{entry_ms}}"}},
	})
	return err
}
