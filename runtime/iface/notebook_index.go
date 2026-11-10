package iface

import (
	"context"
	"fmt"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/gezibash/arc/internal/search"
	"github.com/gezibash/arc/runtime/iface/template"
)

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

func (r *run) saveNotebookIndex(kind, book, text string) error {
	indexKind := r.kindOf(kind).NotebookIndex
	d, err := r.Keyed(indexKind, book)
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

// notebookIndex writes the titles of the pages as they are. The caller
// must clean the index before it shows it: the record that holds it goes
// through sanitize (docs/clients/SPEC.md, section 3.9).
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
			fmt.Fprintf(&out, "| %s | %s | %s | %s | [%s](%s) |\n", number, tableText(p.title), created, stamp(p.updated), p.address, r.pageReference(p)) //arc:raw-ok the index goes to a buffer; the record that holds it goes through sanitize
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
			fmt.Fprintf(&out, "%s- [%s](%s#%s)\n", strings.Repeat("  ", h.level), tableText(h.title), r.pageReference(p), url.PathEscape(h.anchor)) //arc:raw-ok the index goes to a buffer; the record that holds it goes through sanitize
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
		// A new entry changes the version, so search reads the page again.
		var version strings.Builder
		version.WriteString(template.Str(p.entry.rec["id"]))
		for _, e := range p.entries {
			version.WriteString("+" + e.wrap.Hex()[:16])
		}
		sources = append(sources, search.Source{ID: p.entry.d, Version: version.String(), Selected: eligible})
	}
	hits, err := r.env.SearchIndex().Search(r.ctx, search.Request{
		Namespace: r.env.Me().Hex() + "/" + r.in.Author.Hex() + "/" + r.in.Manifest.ID + "/" + n.Kind,
		Sources:   sources, Query: n.Query, Syntax: n.Syntax == "true", Limit: limit,
		Load: func(ctx context.Context, source search.Source) (search.Document, error) {
			p := byID[source.ID]
			scoped := *r
			scoped.ctx = ctx
			text, err := scoped.pageText(p.entry, p.names(), p.entries)
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
			p := byID[hit.ID]
			text, err := r.pageText(e, p.names(), p.entries)
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
