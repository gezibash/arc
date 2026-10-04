package iface

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

type markdownHeading struct {
	title, anchor     string
	level, start, end int
}

// Heading references support ATX and Setext headings outside fenced or
// indented code. They keep duplicate headings addressable by distinct anchors.
func markdownHeadings(body string) []markdownHeading {
	lines := strings.SplitAfter(body, "\n")
	headings := []markdownHeading{}
	used := map[string]bool{}
	fence := byte(0)
	width := 0
	for i, line := range lines {
		text := strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		trimmed := strings.TrimLeft(text, " ")
		if len(text)-len(trimmed) > 3 {
			continue
		}
		if len(trimmed) > 0 && (trimmed[0] == '`' || trimmed[0] == '~') {
			count := 0
			for count < len(trimmed) && trimmed[count] == trimmed[0] {
				count++
			}
			if fence != 0 {
				if trimmed[0] == fence && count >= width && strings.TrimSpace(trimmed[count:]) == "" {
					fence = 0
				}
				continue
			}
			if count >= 3 {
				fence = trimmed[0]
				width = count
				continue
			}
		}
		if fence != 0 {
			continue
		}
		level, start, title := 0, i, ""
		if strings.HasPrefix(trimmed, "#") {
			for level < len(trimmed) && trimmed[level] == '#' {
				level++
			}
			if level > 6 || len(trimmed) > level && trimmed[level] != ' ' && trimmed[level] != '\t' {
				continue
			}
			title = strings.TrimSpace(trimmed[level:])
			if at := strings.LastIndex(title, " "); at >= 0 && strings.Trim(title[at+1:], "#") == "" {
				title = strings.TrimSpace(title[:at])
			}
		} else if i > 0 && trimmed != "" && (strings.Trim(trimmed, "=") == "" || strings.Trim(trimmed, "-") == "") {
			prev := strings.TrimSpace(strings.TrimSuffix(lines[i-1], "\n"))
			if prev == "" || strings.HasPrefix(prev, "#") || strings.HasPrefix(lines[i-1], "    ") {
				continue
			}
			title, start = prev, i-1
			level = 1
			if trimmed[0] == '-' {
				level = 2
			}
		}
		if level == 0 || title == "" {
			continue
		}
		var slug strings.Builder
		for _, c := range strings.ToLower(title) {
			if unicode.IsLetter(c) || unicode.IsDigit(c) || c == '-' || c == '_' {
				slug.WriteRune(c)
			} else if unicode.IsSpace(c) {
				slug.WriteByte('-')
			}
		}
		base := slug.String()
		if base == "" {
			base = "section"
		}
		anchor := base
		for count := 1; used[anchor]; count++ {
			anchor = base + "-" + strconv.Itoa(count)
		}
		used[anchor] = true
		headings = append(headings, markdownHeading{title: title, anchor: anchor, level: level, start: start, end: len(lines)})
	}
	for i := range headings {
		for j := i + 1; j < len(headings); j++ {
			if headings[j].level <= headings[i].level {
				headings[i].end = headings[j].start
				break
			}
		}
	}
	return headings
}

func markdownSection(body, section string) (string, error) {
	lines := strings.SplitAfter(body, "\n")
	for _, h := range markdownHeadings(body) {
		if h.anchor == section || strings.EqualFold(h.title, section) {
			return strings.Join(lines[h.start:h.end], ""), nil
		}
	}
	return "", fmt.Errorf("no Markdown section %q", section)
}
