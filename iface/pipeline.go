package iface

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// pipeline runs the output primitives, in their fixed order. A missing part
// does not stop it: join keeps the text before the part, and the error comes
// back after the output.
func (r *run) pipeline(entries []*entry) ([]*entry, error) {
	o := r.command.Output
	if o.Thread != nil {
		return nil, errors.New("this arc does not run thread yet: it comes in phase C of the interface")
	}

	if o.Open != nil {
		for _, e := range entries {
			if err := open(e.rec, o.Open.Parse); err != nil {
				return nil, err
			}
		}
	}
	if o.Join != nil {
		lines, err := r.template(o.Join.Lines)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			var text string
			var err error
			if lines != "" {
				text, err = r.joinedLines(e, lines)
			} else {
				text, err = r.joined(e)
			}
			if err != nil && r.missing == nil {
				r.missing = err
			}
			e.rec["text"] = text
		}
	}
	if len(o.Where) > 0 {
		var kept []*entry
		for _, e := range entries {
			ok, err := r.matchAll(e.rec, o.Where)
			if err != nil {
				return nil, err
			}
			if ok {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	if o.Latest != nil {
		entries = latest(entries, o.Latest.By)
	}
	if o.Rank != nil {
		query, err := r.template(o.Rank.Query)
		if err != nil {
			return nil, err
		}
		entries = rank(entries, query, o.Rank.Fields, o.Rank.Limit)
	}
	if o.Sort != nil {
		sortEntries(entries, o.Sort.Field, o.Sort.Order == "desc")
	}
	if o.Limit > 0 && len(entries) > o.Limit {
		entries = entries[:o.Limit]
	}
	if o.Tail != nil {
		entries = r.tail(entries)
	}
	return entries, nil
}

// bounds reads a range of lines: a:b, a:, or :b, counted from 1.
func bounds(lines string) (int, int) {
	from, to, _ := strings.Cut(lines, ":")
	first, last := 1, math.MaxInt
	if n, err := strconv.Atoi(from); err == nil {
		first = n
	}
	if n, err := strconv.Atoi(to); err == nil {
		last = n
	}
	return first, last
}

// selectLines keeps a range of lines.
func selectLines(text, lines string) string {
	first, last := bounds(lines)
	all := strings.SplitAfter(text, "\n")
	if all[len(all)-1] == "" {
		all = all[:len(all)-1]
	}
	if first > len(all) {
		return ""
	}
	return strings.Join(all[first-1:min(last, len(all))], "")
}

// field reads a field of a record by its path.
func field(rec Record, path string) any {
	return scope(func(name string) (any, bool) {
		v, ok := rec[name]
		return v, ok
	}).lookup(path)
}

func (r *run) matchAll(rec Record, conds []Cond) (bool, error) {
	for _, c := range conds {
		ok, err := r.match(rec, c)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// match tests one condition. A condition whose value renders empty is
// dropped, so an optional argument can narrow a query or leave it whole.
func (r *run) match(rec Record, c Cond) (bool, error) {
	if len(c.Any) > 0 {
		for _, sub := range c.Any {
			ok, err := r.match(rec, sub)
			if err != nil || ok {
				return ok, err
			}
		}
		return false, nil
	}
	value := field(rec, c.Field)
	for _, test := range []struct {
		template string
		pass     func(want string) bool
	}{
		{c.Is, func(want string) bool { return str(value) == want }},
		{c.Not, func(want string) bool { return str(value) != want }},
		{c.Prefix, func(want string) bool { return strings.HasPrefix(str(value), want) }},
		{c.Lacks, func(want string) bool { return !contains(value, want) }},
	} {
		want, err := r.template(test.template)
		if err != nil {
			return false, err
		}
		if want != "" && !test.pass(want) {
			return false, nil
		}
	}
	return true, nil
}

func contains(value any, want string) bool {
	switch v := value.(type) {
	case []any:
		for _, item := range v {
			if str(item) == want {
				return true
			}
		}
		return false
	case []string:
		for _, item := range v {
			if item == want {
				return true
			}
		}
		return false
	}
	return str(value) == want
}

// latest keeps the newest record for each value of a field.
func latest(entries []*entry, by string) []*entry {
	newest := map[string]*entry{}
	var order []string
	for _, e := range entries {
		key := str(field(e.rec, by))
		old, seen := newest[key]
		if !seen {
			order = append(order, key)
		}
		if !seen || number(e.rec["created"]) > number(old.rec["created"]) {
			newest[key] = e
		}
	}
	out := make([]*entry, 0, len(order))
	for _, key := range order {
		out = append(out, newest[key])
	}
	return out
}

// number reads a value as a number, and reports whether it is one.
func number(v any) float64 {
	n, _ := asNumber(v)
	return n
}

func asNumber(v any) (float64, bool) {
	switch v := v.(type) {
	case int64:
		return float64(v), true
	case int:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}

func sortEntries(entries []*entry, by string, desc bool) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := field(entries[i].rec, by), field(entries[j].rec, by)
		an, aok := asNumber(a)
		bn, bok := asNumber(b)
		less := str(a) < str(b)
		if aok && bok {
			less = an < bn
		}
		if desc {
			return !less && str(a) != str(b)
		}
		return less
	})
}

// words are the lower-case runs of letters and digits of a text.
func words(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// rank orders records by BM25 against a query, with k1 = 1.2 and b = 0.75,
// and keeps the ones that match. It adds the field score.
func rank(entries []*entry, query string, fields []string, limit int) []*entry {
	terms := words(query)
	if len(terms) == 0 {
		return entries
	}
	docs := make([][]string, len(entries))
	total := 0
	frequency := map[string]int{}
	for i, e := range entries {
		var text []string
		for _, f := range fields {
			text = append(text, words(str(field(e.rec, f)))...)
		}
		docs[i] = text
		total += len(text)
		seen := map[string]bool{}
		for _, w := range text {
			if !seen[w] {
				seen[w] = true
				frequency[w]++
			}
		}
	}
	average := float64(total) / float64(max(len(entries), 1))

	const k1, b = 1.2, 0.75
	n := float64(len(entries))
	var out []*entry
	for i, e := range entries {
		counts := map[string]int{}
		for _, w := range docs[i] {
			counts[w]++
		}
		score := 0.0
		for _, term := range terms {
			tf := float64(counts[term])
			if tf == 0 {
				continue
			}
			df := float64(frequency[term])
			idf := math.Log(1 + (n-df+0.5)/(df+0.5))
			score += idf * tf * (k1 + 1) / (tf + k1*(1-b+b*float64(len(docs[i]))/average))
		}
		if score > 0 {
			e.rec["score"] = math.Round(score*1000) / 1000
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return number(out[i].rec["score"]) > number(out[j].rec["score"]) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

// tail shows only what each new version of a record adds to the version
// before it. A version that does not start with the one before is shown
// whole, after a line that says so.
func (r *run) tail(entries []*entry) []*entry {
	if r.seen == nil {
		r.seen = map[string]string{}
	}
	var out []*entry
	for _, e := range entries {
		text := str(e.rec["text"])
		old, seen := r.seen[e.d]
		r.seen[e.d] = text
		switch {
		case !seen:
		case strings.HasPrefix(text, old):
			text = text[len(old):]
			if text == "" {
				continue
			}
		default:
			text = "--- rewritten ---\n" + text
		}
		e.rec["text"] = text
		out = append(out, e)
	}
	return out
}

// save writes one field of each record to a file that must not exist.
func (r *run) save(entries []*entry) error {
	s := r.command.Output.Save
	for _, e := range entries {
		recScope := r.recordScope(e.rec)
		to, err := r.renderIn(s.To, recScope)
		if err != nil {
			return err
		}
		data := []byte(str(field(e.rec, s.Field)))
		if s.Decode == "base64" {
			if data, err = base64.StdEncoding.DecodeString(string(data)); err != nil {
				return fmt.Errorf("the field %s is not base64: %w", s.Field, err)
			}
		}
		if s.SHA256 != "" {
			want, err := r.renderIn(s.SHA256, recScope)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			if got := hex.EncodeToString(sum[:]); got != want {
				return fmt.Errorf("the content hashes to %s, not %s: nothing written", got, want)
			}
		}
		file, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		if _, err := file.Write(data); err != nil {
			file.Close()
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		fmt.Fprintf(r.stdio.Err, "wrote %s, %d bytes\n", to, len(data))
	}
	return nil
}

func (r *run) recordScope(rec Record) scope {
	return func(name string) (any, bool) {
		if value, ok := rec[name]; ok {
			return value, true
		}
		return r.scope(name)
	}
}

func (r *run) renderIn(text string, s scope) (string, error) {
	t, err := compile(text, nil)
	if err != nil {
		return "", err
	}
	return t.render(s, r, false)
}

// streamable says whether a command only shows the whole text of its
// records. Such a command writes each part as it arrives, and a long page
// starts to show before its last part comes.
func (r *run) streamable() bool {
	o := r.command.Output
	if r.flags.JSON || o.Join == nil || o.Where != nil || o.Latest != nil || o.Rank != nil ||
		o.Thread != nil || o.Sort != nil || o.Limit != 0 || o.Tail != nil || o.Save != nil {
		return false
	}
	if o.Format == "" {
		return true
	}
	f := r.in.Manifest.Formats[o.Format]
	return f.Record == "{{text}}" && f.Header == "" && f.Table == nil
}

// stream writes the text of each record, one part at a time.
func (r *run) stream(entries []*entry) error {
	o := r.command.Output
	lines, err := r.template(o.Join.Lines)
	if err != nil {
		return err
	}
	if len(entries) == 0 && o.Format != "" {
		return r.format(r.in.Manifest.Formats[o.Format], nil, r.stdio.Out)
	}
	for _, e := range entries {
		if o.Open != nil {
			if err := open(e.rec, o.Open.Parse); err != nil {
				return err
			}
		}
		ended := true
		write := func(text string) {
			if text != "" {
				io.WriteString(r.stdio.Out, sanitize(text))
				ended = strings.HasSuffix(text, "\n")
			}
		}
		if lines != "" {
			text, err := r.joinedLines(e, lines)
			write(text)
			if err != nil && r.missing == nil {
				r.missing = err
			}
		} else {
			write(str(e.rec["content"]))
			for _, id := range e.parts {
				texts, err := r.partTexts([]string{id})
				if err != nil {
					if r.missing == nil {
						r.missing = fmt.Errorf("a part is missing: sync with a relay or a directory that holds it: %w", err)
					}
					break
				}
				write(texts[0])
			}
		}
		if !ended {
			io.WriteString(r.stdio.Out, "\n")
		}
	}
	return r.missing
}
