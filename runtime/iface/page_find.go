package iface

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/gezibash/arc/sdk/terminal"
)

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
			r.noteLost(pages, pages, pages[i].base)
			return &pages[i], pages, nil
		}
	}
	return nil, nil, noSuchPage(resolved)
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
func (r *run) headD(tmpl, address string) (string, error) {
	return r.scopedWith(Values{"address": address}).template(tmpl)
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
