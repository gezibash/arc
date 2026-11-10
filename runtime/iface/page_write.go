package iface

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/runtime/iface/args"
	"github.com/gezibash/arc/runtime/iface/manifest"
	"github.com/gezibash/arc/runtime/iface/template"
	"github.com/gezibash/arc/sdk/terminal"
)

// writePage makes the head of a new page: its front matter and its first
// text, with a random head id that its entries name. It fills in the front
// matter that the text does not give.
func (r *run) writePage(n *manifest.Notebook, dTemplate string) (string, error) {
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
	title := template.Str(fields["title"])
	switch {
	case n.Title != "" && title != "" && title != n.Title:
		return "", errors.New("frontmatter: title conflicts with --title")
	case title == "" && n.Title != "":
		title = n.Title
	case title == "":
		title = heading(body)
	}
	if isDate(title) && title != n.Title && template.Str(fields["title"]) == "" {
		// A title that the text gives must not change which page today
		// names: only today's page has the date as its title.
		title = ""
	}
	if strings.TrimSpace(body) == "" && title == "" {
		return "", errors.New("no text: give the page on the standard input, or give a --title")
	}
	if title == "" {
		// The first line, and not the date: only today's page has the date
		// as its title, so that today names one page (#398).
		if title = firstLine(body); title == "" {
			return "", errors.New("no title: each line of the text is a date, which only the page of today takes as its title; give a --title")
		}
	}
	for key, want := range map[string]string{"notebook": book, "page": number} {
		if given := template.Str(fields[key]); given != "" && given != want {
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
			if strings.TrimSpace(template.Str(given)) == "" {
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
	scoped := r.scopedWith(args.Values{"head_text": header + body, "head_address": address, "head_title": title, "head_id": hex.EncodeToString(hid)})
	if _, err := scoped.publishSealed(&manifest.Publish{
		Kind: n.Kind, D: d, Revise: "replace", FrontmatterAddress: "{{head_address}}",
		Content: manifest.Content{Text: "{{head_text}}"},
		Tags:    [][]string{{"d", "{{head_address}}"}, {"title", "{{head_title}}"}, {"head", "{{head_id}}"}},
	}); err != nil {
		return "", err
	}
	return address, nil
}

// firstLine is the first line of the text that holds more than blanks,
// with no Markdown heading marker, and cut to 80 characters with an
// ellipsis. It passes a line that is a date: only today's page has the
// date as its title, and a title of the text must not change which page
// today names.
func firstLine(body string) string {
	for line := range strings.SplitSeq(body, "\n") {
		if line = strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), "#")); line != "" && !isDate(line) {
			if r := []rune(line); len(r) > 80 {
				line = string(r[:79]) + "…"
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
func (r *run) appendEntry(n *manifest.Notebook, dTemplate string) (string, error) {
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
func (r *run) fixEntry(n *manifest.Notebook) (string, error) {
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
func (r *run) history(n *manifest.Notebook, dTemplate string) ([]*entry, error) {
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
		checkpoints, err := r.scopedWith(args.Values{"history_d": d}).query(&manifest.Query{Kinds: []string{n.Kind}, Authors: "me", D: "{{history_d}}", History: true})
		if err != nil {
			return nil, err
		}
		for _, e := range checkpoints {
			// The checkpoint of a lost head belongs to its own page.
			if page.hid == "" || template.Str(field(e.rec, "tags.head")) == page.hid {
				out = append(out, e)
			}
		}
	}
	for _, e := range out {
		e.rec["at"] = stamp(checkpointAt(e))
		e.rec["fixes"] = ""
	}
	for _, p := range page.entries {
		out = append(out, p.e)
	}
	return out, nil
}

// isDate says whether text is a date, as the title of today's page is.
func isDate(text string) bool {
	_, err := time.Parse("2006-01-02", text)
	return err == nil
}

// checkpointAt is the time of a version of a head: the updated_at of its
// front matter, in milliseconds as read and ls show it, else the second of
// its event.
func checkpointAt(e *entry) time.Time {
	if fields, _, err := frontmatter(template.Str(e.rec["content"])); err == nil {
		if at, err := time.Parse(time.RFC3339Nano, template.Str(fields["updated_at"])); err == nil {
			return at
		}
	}
	return time.Unix(int64(number(e.rec["created"])), 0)
}
