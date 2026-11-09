package iface

import (
	"fmt"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/draft"
)

// show runs the pipeline on entries and writes them. Then the exit rules of
// the output read the first entry.
func (r *run) show(entries []*entry) error {
	entries, err := r.pipeline(entries)
	if err != nil {
		return err
	}
	if err := r.write(entries, r.stdio.Out); err != nil {
		return err
	}
	return r.exit(entries)
}

// ExitError asks for an exit status. The command wrote its output already,
// so the caller exits with Code, and writes no message.
type ExitError struct{ Code int }

func (e ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// ExitCode is the exit status.
func (e ExitError) ExitCode() int { return e.Code }

// exit applies the first exit rule whose conditions the first entry meets.
// With no entry, or no rule that fits, the command has no exit status.
func (r *run) exit(entries []*entry) error {
	rules := r.command.Output.Exit
	if len(rules) == 0 || len(entries) == 0 {
		return nil
	}
	rec := entries[0].rec
	for _, rule := range rules {
		ok, err := r.matchAll(rec, rule.Where)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		text, err := r.renderIn(rule.Code, r.recordScope(rec))
		if err != nil {
			return err
		}
		code, err := strconv.Atoi(strings.TrimSpace(text))
		if err != nil || code < 0 || code > 255 {
			code = 1
		}
		if code == 0 {
			return nil
		}
		return ExitError{Code: code}
	}
	return nil
}

// partTexts opens parts by their IDs, in order. It stops at the first part
// that it cannot find, and returns the texts before it with an error.
func (r *run) partTexts(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	want := make([]nostr.ID, 0, len(ids))
	for _, id := range ids {
		parsed, err := nostr.IDFromHex(id)
		if err != nil {
			return nil, fmt.Errorf("the parts tag names %q, which is not an event", id)
		}
		want = append(want, parsed)
	}
	// Naming the kind and the citizen keeps out a part of another citizen.
	filter := r.selfFilter([]nostr.Kind{draft.PartKind}, nil)
	filter.IDs = want
	events, err := r.fetchSelf(filter)
	if err != nil {
		return nil, err
	}
	byID := map[nostr.ID]nostr.Event{}
	for _, event := range events {
		byID[event.ID] = event
	}
	var out []string
	for i, id := range want {
		event, ok := byID[id]
		if !ok {
			return out, fmt.Errorf("part %d of %d is missing: sync with a relay or a directory that holds it", i+1, len(want))
		}
		text, err := r.opener().OpenPart(r.ctx, event)
		if err != nil {
			return out, fmt.Errorf("part %d of %d: %w", i+1, len(want), err)
		}
		out = append(out, text)
	}
	return out, nil
}

// joined is the whole text of an entry: its content, then its parts.
func (r *run) joined(e *entry) (string, error) {
	texts, err := r.partTexts(e.parts)
	return str(e.rec["content"]) + strings.Join(texts, ""), err
}

// joinedLines is a range of lines of an entry. When the entry counts its
// lines, it fetches only the parts that hold the range.
func (r *run) joinedLines(e *entry, lines string) (string, error) {
	if e.lines == nil {
		text, err := r.joined(e)
		return selectLines(text, lines), err
	}
	first, last := bounds(lines)
	// Piece i holds lines start[i] to start[i]+lines[i]; the last of them can
	// go on in the next piece.
	start := 1
	var picked []int
	pickedStart := 0
	for i, n := range e.lines {
		if start <= last && start+n >= first {
			if picked == nil {
				pickedStart = start
			}
			picked = append(picked, i)
		}
		start += n
	}
	var b strings.Builder
	var ids []string
	for _, i := range picked {
		if i == 0 {
			b.WriteString(str(e.rec["content"]))
		} else {
			ids = append(ids, e.parts[i-1])
		}
	}
	texts, err := r.partTexts(ids)
	b.WriteString(strings.Join(texts, ""))
	if picked == nil {
		return "", err
	}
	shift := pickedStart - 1
	return selectLines(b.String(), fmt.Sprintf("%d:%d", max(first-shift, 1), last-shift)), err
}
