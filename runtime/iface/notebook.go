package iface

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/runtime/iface/template"
)

// Notebook is an application view over sealed pages, not a transport or a
// second persistence engine. Page and index events use the normal draft core.
type Notebook struct {
	Op       string `json:"op"`
	Kind     string `json:"kind"`
	D        string `json:"d,omitempty"`
	Address  string `json:"address,omitempty"`
	Notebook string `json:"notebook,omitempty"`
	Prefix   string `json:"prefix,omitempty"`
	Query    string `json:"query,omitempty"`
	Syntax   string `json:"syntax,omitempty"`
	Title    string `json:"title,omitempty"`
	Page     string `json:"page,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Order    string `json:"order,omitempty"`
	Reverse  string `json:"reverse,omitempty"`
	Limit    string `json:"limit,omitempty"`
	Section  string `json:"section,omitempty"`
	// Text is the text of a write, an append or a fix.
	Text string `json:"text,omitempty"`
}

var notebookPageName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

var notebookPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*$`)

func (m *Manifest) checkNotebook(n *Notebook, check, checkD func(string) error) error {
	if err := m.checkKind(n.Kind); err != nil {
		return err
	}
	if m.Kinds[n.Kind].NotebookIndex == "" {
		return errors.New("notebook action requires a notebook_index kind")
	}
	ops := []string{"read", "list", "search", "select", "next", "prev", "index", "toc"}
	writes := []string{"history", "write", "append", "fix", "delete"}
	if m.Kinds[n.Kind].NotebookEntry != "" {
		ops = append(ops, writes...)
	}
	if !slices.Contains(ops, n.Op) {
		if slices.Contains(writes, n.Op) {
			return fmt.Errorf("notebook %s requires a notebook_entry kind", n.Op)
		}
		return fmt.Errorf("invalid notebook operation %q", n.Op)
	}
	if slices.Contains(writes, n.Op) && (n.D == "" || n.Address == "") {
		return fmt.Errorf("notebook %s requires d and address", n.Op)
	}
	if n.Op == "read" && (n.D == "" || n.Address == "") {
		return errors.New("notebook read requires d and address")
	}
	if (n.Op == "next" || n.Op == "prev") && n.Address == "" {
		return errors.New("notebook navigation requires address")
	}
	if (n.Op == "index" || n.Op == "toc") && n.Notebook == "" {
		return errors.New("notebook index or toc requires notebook")
	}
	if err := checkD(n.D); err != nil {
		return err
	}
	for _, text := range []string{n.Address, n.Notebook, n.Prefix, n.Query, n.Syntax, n.Title, n.Page, n.From, n.To, n.Order, n.Reverse, n.Limit, n.Section, n.Text} {
		if err := check(text); err != nil {
			return err
		}
	}
	return nil
}

func ordinal(text string) (int64, error) {
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil || n < 1 || strconv.FormatInt(n, 10) != text {
		return 0, fmt.Errorf("frontmatter: page must be a canonical positive integer, got %q", text)
	}
	return n, nil
}

// notebookHeader preserves supplied content and optional metadata. The two
// timestamps are managed by ARC; caller values cannot change original creation.
func (r *run) notebookHeader(p *Publish, content string, old *draft.Draft) (string, error) {
	if r.kindOf(p.Kind).NotebookIndex == "" {
		return content, nil
	}
	fields, body, err := frontmatter(content)
	if err != nil {
		return "", err
	}
	for _, key := range []string{"title", "page", "notebook"} {
		if strings.TrimSpace(template.Str(fields[key])) == "" {
			return "", fmt.Errorf("frontmatter: requires nonempty %q", key)
		}
	}
	if _, err = ordinal(template.Str(fields["page"])); err != nil {
		return "", err
	}
	if !notebookPattern.MatchString(template.Str(fields["notebook"])) {
		return "", errors.New("frontmatter: notebook must be project/notebook")
	}
	now := r.env.Now().UTC().Format(timeFormat)
	created := now
	if old != nil && !old.Deleted {
		before, _, err := frontmatter(old.Event.Content)
		if err != nil {
			return "", err
		}
		if stamp := template.Str(before["created_at"]); stamp != "" {
			if _, err = time.Parse(time.RFC3339Nano, stamp); err != nil {
				return "", errors.New("stored notebook created_at is invalid")
			}
			created = stamp
		} else {
			// For a historical numbered page, use the earliest available checkpoint.
			saved := *r
			saved.values = Values{"notebook_existing_d": old.D}
			history, err := saved.query(&Query{Kinds: []string{p.Kind}, Authors: "me", D: "{{notebook_existing_d}}", History: true})
			if err != nil {
				return "", err
			}
			first := int64(old.Event.CreatedAt)
			for _, e := range history {
				if stamp := int64(number(e.rec["created"])); stamp < first {
					first = stamp
				}
			}
			created = time.Unix(first, 0).UTC().Format(timeFormat)
		}
	}
	header := content[:len(content)-len(body)]
	newline := "\n"
	if strings.HasPrefix(header, "---\r\n") {
		newline = "\r\n"
	}
	lines := strings.Split(strings.ReplaceAll(header, "\r\n", "\n"), "\n")
	kept := []string{"---"}
	for _, line := range lines[1:] {
		if line == "---" {
			break
		}
		key, _, _ := strings.Cut(line, ":")
		if key != "created_at" && key != "updated_at" {
			kept = append(kept, line)
		}
	}
	kept = append(kept, "created_at: "+created, "updated_at: "+now, "---")
	return strings.Join(kept, newline) + newline + body, nil
}

type notebookPage struct {
	entry                *entry
	address, title, book string
	page                 int64
	created, updated     time.Time
	text                 string
	// entries are the entries of the page, oldest first.
	entries []pageEntry
	// base is the address that the head and the entries name, and hid the
	// id of the head. A lost page has an address of its own,
	// <base>-<hid>, and checkpoint is the ID of its head, if it has one.
	base, hid  string
	lost       bool
	checkpoint nostr.ID
}

func (r *run) notebookPages(kind string) ([]notebookPage, error) {
	entries, err := r.query(&Query{Kinds: []string{kind}, Authors: "me"})
	if err != nil {
		return nil, err
	}
	pages := []notebookPage{}
	for _, e := range entries {
		address := template.Str(field(e.rec, "tags.d"))
		slash := strings.LastIndex(address, "/")
		if slash < 0 || !notebookPattern.MatchString(address[:slash]) {
			continue
		}
		p := notebookPage{entry: e, address: address, base: address, hid: template.Str(field(e.rec, "tags.head"))}
		pageMeta(&p)
		pages = append(pages, p)
	}
	if r.kindOf(kind).NotebookEntry == "" {
		return pages, nil
	}
	return r.attachEntries(kind, pages)
}

// pageMeta reads the title, the number and the times of a page from its
// head, and puts them in its record.
func pageMeta(p *notebookPage) {
	e := p.entry
	p.book = p.base[:strings.LastIndex(p.base, "/")]
	p.title = template.Str(field(e.rec, "tags.title"))
	fields, _, parseErr := frontmatter(template.Str(e.rec["content"]))
	if parseErr == nil && template.Str(fields["notebook"])+"/"+template.Str(fields["page"]) == p.base {
		if !p.lost {
			p.page, _ = ordinal(template.Str(fields["page"]))
		}
		p.created, _ = time.Parse(time.RFC3339Nano, template.Str(fields["created_at"]))
		p.updated, _ = time.Parse(time.RFC3339Nano, template.Str(fields["updated_at"]))
	}
	if p.updated.IsZero() && number(e.rec["created"]) > 0 {
		p.updated = time.Unix(int64(number(e.rec["created"])), 0).UTC()
	}
	if len(p.entries) > 0 {
		if p.created.IsZero() {
			p.created = p.entries[0].at()
		}
		// The time of an entry is in milliseconds: keep the head's time when
		// the entry is not later.
		if newest := p.entries[len(p.entries)-1].at(); newest.After(p.updated) {
			p.updated = newest
		}
	}
	e.rec["page"] = p.page
	e.rec["notebook"] = p.book
	e.rec["created_at"] = stamp(p.created)
	e.rec["updated_at"] = stamp(p.updated)
	e.rec["legacy"] = p.page == 0 || p.created.IsZero()
}

// timeFormat is the one form of a time of the journal: UTC, with three
// digits of milliseconds, so that times line up (#400). Reads also take
// RFC 3339 times of other precision, as older pages have them.
const timeFormat = "2006-01-02T15:04:05.000Z07:00"

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(timeFormat)
}

// dateBound treats --to YYYY-MM-DD as the whole UTC day, not just midnight.
func dateBound(value string, end bool) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse("2006-01-02", value); err == nil {
		if end {
			t = t.AddDate(0, 0, 1).Add(-time.Nanosecond)
		}
		return t, nil
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("date %q must be YYYY-MM-DD or RFC3339", value)
	}
	return t, nil
}

func (r *run) notebookAddress(value string) (string, string, error) {
	address, section := value, ""
	if strings.HasPrefix(value, r.in.Manifest.ID+"+arc://") {
		u, err := url.Parse(value)
		if err != nil || u.Host != r.env.Me().Hex() || u.RawQuery != "" || u.User != nil {
			return "", "", errors.New("notebook reference must name this identity")
		}
		address, section = strings.TrimPrefix(u.Path, "/"), u.Fragment
	} else {
		address, section, _ = strings.Cut(value, "#")
	}
	slash := strings.LastIndex(address, "/")
	if slash < 0 || !notebookPattern.MatchString(address[:slash]) || !notebookPageName.MatchString(address[slash+1:]) {
		return "", "", fmt.Errorf("invalid page address %q", address)
	}
	return address, section, nil
}

func (r *run) notebookAction(n *Notebook) ([]*entry, error) {
	copy := *n
	n = &copy
	dTemplate := n.D
	values := []*string{&n.Address, &n.Notebook, &n.Prefix, &n.Query, &n.Syntax, &n.Title, &n.Page, &n.From, &n.To, &n.Order, &n.Reverse, &n.Limit, &n.Section, &n.Text}
	for _, value := range values {
		text, err := r.template(*value)
		if err != nil {
			return nil, err
		}
		*value = text
	}
	switch n.Op {
	case "write", "append", "fix", "history", "delete":
		return r.notebookWrite(n, dTemplate)
	case "read":
		address, anchor, err := r.notebookAddress(n.Address)
		if err != nil {
			return nil, err
		}
		if n.Section == "" {
			n.Section = anchor
		}
		if r.kindOf(n.Kind).NotebookEntry != "" {
			p, err := r.findPage(n.Kind, address, "today")
			if err != nil {
				return nil, err
			}
			page := []notebookPage{*p}
			if err := r.compose(page); err != nil {
				return nil, err
			}
			page[0].entry.rec["entries"] = entryList(p.entries)
			return r.notebookSections([]*entry{page[0].entry}, n.Section)
		}
		// Resolve the input reference before the manifest's keyed d template.
		d, err := r.headD(dTemplate, address)
		if err != nil {
			return nil, err
		}
		head, err := r.head(n.Kind, d)
		if err != nil {
			return nil, err
		}
		if head == nil {
			return nil, noSuchPage(address)
		}
		return r.notebookSections([]*entry{head}, n.Section)
	}
	if n.Notebook != "" && !notebookPattern.MatchString(n.Notebook) {
		return nil, errors.New("notebook must be project/notebook")
	}
	from, err := dateBound(n.From, false)
	if err != nil {
		return nil, err
	}
	to, err := dateBound(n.To, true)
	if err != nil {
		return nil, err
	}
	if !from.IsZero() && !to.IsZero() && from.After(to) {
		return nil, errors.New("from must not be after to")
	}
	page := int64(0)
	if n.Page != "" {
		page, err = ordinal(n.Page)
		if err != nil {
			return nil, err
		}
	}
	limit := 0
	if n.Limit != "" {
		limit, err = strconv.Atoi(n.Limit)
		if err != nil || limit < 1 {
			return nil, errors.New("limit must be positive")
		}
	}
	order := n.Order
	if order == "" {
		order = "created"
	}
	if !slices.Contains([]string{"created", "updated", "page"}, order) {
		return nil, errors.New("order must be created, updated or page")
	}
	navPage := int64(0)
	if n.Op == "next" || n.Op == "prev" {
		address, _, err := r.notebookAddress(n.Address)
		if err != nil {
			return nil, err
		}
		slash := strings.LastIndex(address, "/")
		n.Notebook = address[:slash]
		navPage, err = ordinal(address[slash+1:])
		if err != nil {
			return nil, err
		}
		order = "page"
	}
	bodies := n.Op == "index" || n.Op == "toc"
	pages, err := r.notebookPages(n.Kind)
	if err != nil {
		return nil, err
	}
	allPages := slices.Clone(pages)
	selected := pages[:0]
	for _, p := range pages {
		if n.Notebook != "" && p.book != n.Notebook || n.Prefix != "" && !strings.HasPrefix(p.address, n.Prefix) || page != 0 && p.page != page || n.Title != "" && !strings.Contains(strings.ToLower(p.title), strings.ToLower(n.Title)) {
			continue
		}
		if (!from.IsZero() || !to.IsZero()) && p.created.IsZero() {
			continue
		}
		if !from.IsZero() && p.created.Before(from) || !to.IsZero() && p.created.After(to) {
			continue
		}
		if n.Op == "next" && (p.page == 0 || p.page <= navPage) || n.Op == "prev" && (p.page == 0 || p.page >= navPage) {
			continue
		}
		selected = append(selected, p)
	}
	pages = selected
	if n.Op == "search" {
		return r.searchNotebook(n, allPages, pages, limit)
	}
	sortNotebookPages(pages, order, n.Reverse == "true" || n.Op == "prev")
	if n.Op == "next" || n.Op == "prev" {
		if len(pages) == 0 {
			return nil, fmt.Errorf("no %s page in %s", n.Op, n.Notebook)
		}
		pages = pages[:1]
	}
	if n.Op != "search" && limit > 0 && len(pages) > limit {
		pages = pages[:limit]
	}
	// Fetch parts only after metadata filters and non-search limits. A missing
	// part in another notebook must not prevent a selected notebook from reading.
	if r.kindOf(n.Kind).NotebookEntry != "" {
		if err := r.compose(pages); err != nil {
			return nil, err
		}
	}
	if bodies {
		for i := range pages {
			pages[i].text, err = r.joined(pages[i].entry)
			if err != nil {
				return nil, err
			}
		}
	}
	if n.Op == "index" || n.Op == "toc" {
		// Always derive from source pages. Stored indexes can be stale after an
		// offline concurrent write; rebuilding never trusts the cached snapshot.
		text := r.notebookIndex(n.Notebook, pages, n.Op == "index")
		if n.Op == "index" && !r.flags.DryRun {
			if err := r.saveNotebookIndex(n.Kind, n.Notebook, text); err != nil {
				return nil, err
			}
		}
		return []*entry{{rec: Record{"content": text, "text": text}}}, nil
	}
	entries := make([]*entry, 0, len(pages))
	for _, p := range pages {
		if bodies {
			p.entry.rec["text"] = p.text
		}
		entries = append(entries, p.entry)
	}
	if n.Op == "select" || n.Op == "next" || n.Op == "prev" {
		return r.notebookSections(entries, n.Section)
	}
	if n.Op == "list" {
		// Only the pages that ls shows get a notice.
		r.noteLost(allPages, pages, "")
		// A failed write leaves no page to list, so name its address here.
		if err := r.reportUnnamed(n.Kind); err != nil {
			return nil, err
		}
	}
	return entries, nil
}
