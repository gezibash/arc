// Package specs holds the rules of docs/SPEC-TEMPLATE.md that a program can
// check: how to read the header of a spec. The tests of this package check
// the documents under docs/.
package specs

import (
	"path"
	"regexp"
	"strings"
)

var (
	itemLine = regexp.MustCompile(`^- (Status|Layers|Owns|Proof|Remaining|Unverified): (.*)$`)
)

// visible returns the lines of a Markdown document. A line inside a fenced
// code block comes back empty, so an example never reads as the document.
func visible(text string) []string {
	lines := strings.Split(text, "\n")
	fenced := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			fenced = !fenced
			lines[i] = ""
			continue
		}
		if fenced {
			lines[i] = ""
		}
	}
	return lines
}

// header returns the items of the list that follows the title, and the names
// of the items that appear more than once.
func header(lines []string) (items map[string]string, repeated []string) {
	items = map[string]string{}
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			break
		}
		if match := itemLine.FindStringSubmatch(line); match != nil {
			if _, seen := items[match[1]]; seen {
				repeated = append(repeated, match[1])
			}
			items[match[1]] = strings.TrimSpace(match[2])
		}
	}
	return items, repeated
}

// list splits a comma list.
func list(value string) []string {
	var out []string
	for part := range strings.SplitSeq(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// Owns returns the packages that a spec names in its Owns item.
func Owns(text string) []string {
	items, _ := header(visible(text))
	if items["Owns"] == "none" {
		return nil
	}
	return list(items["Owns"])
}

// Unowned returns the packages under core/ and sdk/ that
// docs/GRANDFATHERED.md lists: the packages that no spec owns yet.
func Unowned(text string) []string {
	return table(text, "## Packages without a spec")
}

// table returns the first cell of each row of the Markdown table that follows
// a heading.
func table(text, heading string) []string {
	var cells []string
	inside := false
	for _, line := range visible(text) {
		if strings.HasPrefix(line, "## ") {
			inside = line == heading
			continue
		}
		if !inside || !strings.HasPrefix(line, "| ") {
			continue
		}
		cell := strings.TrimSpace(strings.Split(line, "|")[1])
		if cell != "" && !strings.HasPrefix(cell, "---") && cell != "Spec" && cell != "Package" {
			cells = append(cells, strings.Trim(cell, "`"))
		}
	}
	return cells
}

// isSpec reports whether a file is a spec: a SPEC.md under docs/.
func isSpec(file string) bool {
	return strings.HasPrefix(file, "docs/") && path.Base(file) == "SPEC.md"
}
