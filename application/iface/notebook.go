package iface

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gezibash/arc/core/draft"
	"github.com/gezibash/arc/internal/search"
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
}

var notebookPageName = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)

var notebookPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*/[a-z0-9][a-z0-9_.-]*$`)

func (m *Manifest) checkNotebook(n *Notebook, check func(string) error) error {
	if err := m.checkKind(n.Kind); err != nil {
		return err
	}
	if m.Kinds[n.Kind].NotebookIndex == "" {
		return errors.New("notebook action requires a notebook_index kind")
	}
	if !slices.Contains([]string{"read", "list", "search", "select", "next", "prev", "index", "toc"}, n.Op) {
		return fmt.Errorf("invalid notebook operation %q", n.Op)
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
	for _, text := range []string{n.D, n.Address, n.Notebook, n.Prefix, n.Query, n.Syntax, n.Title, n.Page, n.From, n.To, n.Order, n.Reverse, n.Limit, n.Section} {
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
		if strings.TrimSpace(str(fields[key])) == "" {
			return "", fmt.Errorf("frontmatter: requires nonempty %q", key)
		}
	}
	if _, err = ordinal(str(fields["page"])); err != nil {
		return "", err
	}
	if !notebookPattern.MatchString(str(fields["notebook"])) {
		return "", errors.New("frontmatter: notebook must be project/notebook")
	}
	now := r.env.Now().UTC().Format(time.RFC3339Nano)
	created := now
	if old != nil && !old.Deleted {
		before, _, err := frontmatter(old.Event.Content)
		if err != nil {
			return "", err
		}
		if stamp := str(before["created_at"]); stamp != "" {
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
			created = time.Unix(first, 0).UTC().Format(time.RFC3339Nano)
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
}

func (r *run) notebookPages(kind string) ([]notebookPage, error) {
	entries, err := r.query(&Query{Kinds: []string{kind}, Authors: "me"})
	if err != nil {
		return nil, err
	}
	pages := []notebookPage{}
	for _, e := range entries {
		address := str(field(e.rec, "tags.d"))
		slash := strings.LastIndex(address, "/")
		if slash < 0 || !notebookPattern.MatchString(address[:slash]) {
			continue
		}
		p := notebookPage{entry: e, address: address, book: address[:slash], title: str(field(e.rec, "tags.title"))}
		fields, _, parseErr := frontmatter(str(e.rec["content"]))
		if parseErr == nil && str(fields["notebook"])+"/"+str(fields["page"]) == address {
			p.page, _ = ordinal(str(fields["page"]))
			p.created, _ = time.Parse(time.RFC3339Nano, str(fields["created_at"]))
			p.updated, _ = time.Parse(time.RFC3339Nano, str(fields["updated_at"]))
		}
		if p.updated.IsZero() {
			p.updated = time.Unix(int64(number(e.rec["created"])), 0).UTC()
		}
		e.rec["page"] = p.page
		e.rec["notebook"] = p.book
		e.rec["created_at"] = stamp(p.created)
		e.rec["updated_at"] = stamp(p.updated)
		e.rec["legacy"] = p.page == 0 || p.created.IsZero()
		pages = append(pages, p)
	}
	return pages, nil
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
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
	values := []*string{&n.Address, &n.Notebook, &n.Prefix, &n.Query, &n.Syntax, &n.Title, &n.Page, &n.From, &n.To, &n.Order, &n.Reverse, &n.Limit, &n.Section}
	for _, value := range values {
		text, err := r.template(*value)
		if err != nil {
			return nil, err
		}
		*value = text
	}
	if n.Op == "read" {
		address, anchor, err := r.notebookAddress(n.Address)
		if err != nil {
			return nil, err
		}
		if n.Section == "" {
			n.Section = anchor
		}
		// Resolve the input reference before the manifest's keyed d template.
		scoped := *r
		scoped.values = maps.Clone(r.values)
		scoped.values["address"] = address
		d, err := scoped.template(dTemplate)
		if err != nil {
			return nil, err
		}
		entries, err := r.query(&Query{Kinds: []string{n.Kind}, Authors: "me", D: d, Limit: "1"})
		if err != nil {
			return nil, err
		}
		return r.notebookSections(entries, n.Section)
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
	return entries, nil
}

func (r *run) notebookSections(entries []*entry, section string) ([]*entry, error) {
	if section == "" {
		return entries, nil
	}
	for _, e := range entries {
		text, err := r.joined(e)
		if err != nil {
			return nil, err
		}
		_, body, err := frontmatter(text)
		if err != nil {
			return nil, err
		}
		text, err = markdownSection(body, section)
		if err != nil {
			return nil, err
		}
		e.rec["content"] = text
		e.parts = nil
		e.lines = nil
	}
	return entries, nil
}

func (r *run) refreshNotebookIndex(kind, book string) error {
	pages, err := r.notebookPages(kind)
	if err != nil {
		return err
	}
	selected := pages[:0]
	for _, p := range pages {
		if p.book == book {
			p.text, err = r.joined(p.entry)
			if err != nil {
				return err
			}
			selected = append(selected, p)
		}
	}
	sortNotebookPages(selected, "page", false)
	return r.saveNotebookIndex(kind, book, r.notebookIndex(book, selected, true))
}

func (r *run) saveNotebookIndex(kind, book, text string) error {
	indexKind := r.kindOf(kind).NotebookIndex
	d, err := r.keyed(indexKind, book)
	if err != nil {
		return err
	}
	scoped := *r
	scoped.values = Values{"notebook_index_content": text}
	_, err = scoped.publishSealed(&Publish{Kind: indexKind, D: d, Content: Content{Text: "{{notebook_index_content}}"}, Revise: "replace", Tags: [][]string{{"d", book}, {"notebook", book}, {"title", "Index of " + book}}})
	return err
}

func tableText(value string) string {
	return strings.NewReplacer("|", "\\|", "\n", " ", "\r", " ", "[", "\\[", "]", "\\]").Replace(value)
}

func (r *run) pageReference(p notebookPage) string {
	return r.in.Manifest.ID + "+arc://" + r.env.Me().Hex() + "/" + p.address
}

func (r *run) notebookIndex(book string, pages []notebookPage, table bool) string {
	var out strings.Builder
	fmt.Fprintf(&out, "# %s\n\n", book)
	if table {
		out.WriteString("## Index\n\n| Page | Title | Created (UTC) | Updated (UTC) | Reference |\n| --- | --- | --- | --- | --- |\n")
		for _, p := range pages {
			number := "legacy"
			if p.page > 0 {
				number = strconv.FormatInt(p.page, 10)
			}
			created := stamp(p.created)
			if created == "" {
				created = "unknown"
			}
			fmt.Fprintf(&out, "| %s | %s | %s | %s | [%s](%s) |\n", number, tableText(p.title), created, stamp(p.updated), p.address, r.pageReference(p))
		}
		out.WriteString("\n")
	}
	out.WriteString("## Table of contents\n\n")
	for _, p := range pages {
		label := p.title
		if p.page > 0 {
			label = strconv.FormatInt(p.page, 10) + ". " + label
		}
		fmt.Fprintf(&out, "- [%s](%s)\n", tableText(label), r.pageReference(p))
		_, body, err := frontmatter(p.text)
		if err != nil {
			body = p.text
		}
		for _, h := range markdownHeadings(body) {
			fmt.Fprintf(&out, "%s- [%s](%s#%s)\n", strings.Repeat("  ", h.level), tableText(h.title), r.pageReference(p), url.PathEscape(h.anchor))
		}
	}
	return out.String()
}

func sortNotebookPages(pages []notebookPage, order string, reverse bool) {
	sort.Slice(pages, func(i, j int) bool {
		a, b := pages[i], pages[j]
		if order == "page" && a.page != b.page {
			if a.page == 0 {
				return false
			}
			if b.page == 0 {
				return true
			}
			return a.page < b.page
		}
		first, second := a.created, b.created
		if order == "updated" {
			first, second = a.updated, b.updated
		}
		if !first.Equal(second) {
			if first.IsZero() {
				return false
			}
			if second.IsZero() {
				return true
			}
			return first.Before(second)
		}
		return a.address < b.address
	})

	if reverse {
		slices.Reverse(pages)
	}
}

// searchNotebook keeps notebook semantics here. The adapter sees only generic
// versions, selected IDs and the text of changed selected documents.
func (r *run) searchNotebook(n *Notebook, all, selected []notebookPage, limit int) ([]*entry, error) {
	byID := make(map[string]notebookPage, len(selected))
	for _, p := range selected {
		byID[p.entry.d] = p
	}
	sources := make([]search.Source, 0, len(all))
	for _, p := range all {
		_, eligible := byID[p.entry.d]
		sources = append(sources, search.Source{ID: p.entry.d, Version: str(p.entry.rec["id"]), Selected: eligible})
	}
	hits, err := r.env.SearchIndex().Search(r.ctx, search.Request{
		Namespace: r.env.Me().Hex() + "/" + r.in.Author.Hex() + "/" + r.in.Manifest.ID + "/" + n.Kind,
		Sources:   sources, Query: n.Query, Syntax: n.Syntax == "true", Limit: limit,
		Load: func(ctx context.Context, source search.Source) (search.Document, error) {
			p := byID[source.ID]
			scoped := *r
			scoped.ctx = ctx
			text, err := scoped.joined(p.entry)
			if err != nil {
				return search.Document{}, err
			}
			_, body, err := frontmatter(text)
			if err != nil {
				body = text
			} // Historical headerless pages remain searchable.
			return search.Document{Title: p.title, Text: body}, nil
		},
	})
	if err != nil {
		return nil, err
	}
	entries := make([]*entry, 0, len(hits))
	for _, hit := range hits {
		e := byID[hit.ID].entry
		if r.flags.JSON {
			text, err := r.joined(e)
			if err != nil {
				return nil, err
			}
			e.rec["text"] = text
		}
		e.rec["score"] = hit.Score
		entries = append(entries, e)
	}
	return entries, nil
}
