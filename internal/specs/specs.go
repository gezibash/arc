// Package specs holds the rules of docs/SPEC-TEMPLATE.md that a program can
// check: how to read a spec, and what a pull request can change. The tests of
// this package check the documents under docs/. The command prpolicy checks a
// pull request.
package specs

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

var (
	itemLine  = regexp.MustCompile(`^- (Status|Layers|Owns|Proof|Remaining|Unverified): (.*)$`)
	gateLine  = regexp.MustCompile(`^### ([A-Z][0-9]+)\. `)
	reference = regexp.MustCompile(`(?m)^Spec: (docs/[\w./-]+/SPEC\.md)\b`)
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

// gates returns the text of each answered gate of a spec, by ID, and whether
// the spec has a Gates section.
func gates(lines []string) (answers map[string][]string, found bool) {
	answers = map[string][]string{}
	inside, id := false, ""
	for _, line := range lines {
		if strings.HasPrefix(line, "## ") {
			inside, id = line == "## Gates", ""
			found = found || inside
			continue
		}
		if !inside {
			continue
		}
		if match := gateLine.FindStringSubmatch(line); match != nil {
			id = match[1]
			answers[id] = []string{}
			continue
		}
		if id != "" {
			answers[id] = append(answers[id], line)
		}
	}
	return answers, found
}

// Owns returns the packages that a spec names in its Owns item.
func Owns(text string) []string {
	items, _ := header(visible(text))
	if items["Owns"] == "none" {
		return nil
	}
	return list(items["Owns"])
}

// Record returns the lines of the Gates section of a spec, without the blank
// lines, and whether the spec has the section. The record keeps the lines of
// a code block: an answer can quote one.
func Record(text string) (lines []string, found bool) {
	shown := visible(text)
	inside := false
	for i, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(shown[i], "## ") {
			inside = shown[i] == "## Gates"
			found = found || inside
			continue
		}
		if line = strings.TrimRight(line, " \t\r"); inside && line != "" {
			lines = append(lines, line)
		}
	}
	return lines, found
}

// AppendOnly returns the first line of the old record that the new record
// does not hold in its place, or "" if the new record only adds lines.
func AppendOnly(base, head []string) string {
	next := 0
	for _, line := range base {
		at := slices.Index(head[next:], line)
		if at < 0 {
			return line
		}
		next += at + 1
	}
	return ""
}

// Grandfathered returns the two lists of docs/GRANDFATHERED.md: the specs
// that predate the gates, and the packages that no spec owns.
func Grandfathered(text string) (specs, packages []string) {
	return table(text, "## Specs without gates"), table(text, "## Packages without a spec")
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

// References returns the specs that the description of a pull request names,
// each in a line "Spec: docs/<name>/SPEC.md".
func References(body string) []string {
	var specs []string
	for _, match := range reference.FindAllStringSubmatch(strings.ReplaceAll(body, "\r\n", "\n"), -1) {
		specs = append(specs, match[1])
	}
	return specs
}

// Change is one changed file of a pull request.
type Change struct {
	// Path is the path at the head. Previous is the path at the base, if the
	// pull request renamed the file.
	Path, Previous string
	// Removed says that the file does not exist at the head.
	Removed bool
}

// PullRequest is what the policy reads of one pull request. Base and Head
// read one file of the base branch and of the pull request.
type PullRequest struct {
	Body       string
	Changes    []Change
	Base, Head func(path string) (text string, ok bool)
}

const (
	grandfatheredPath = "docs/GRANDFATHERED.md"
	templatePath      = "docs/SPEC-TEMPLATE.md"
)

func isSpec(file string) bool {
	return strings.HasPrefix(file, "docs/") && path.Base(file) == "SPEC.md"
}

// isDocument reports a file that can hold a record of gates: a spec, or a
// proposal on its way to one.
func isDocument(file string) bool {
	return strings.HasPrefix(file, "docs/") && strings.HasSuffix(file, ".md")
}

// protected returns the package of a file under core/ or sdk/ whose change
// needs a spec, or "". A test, its data, and a doc.go need none: the tests of
// this package check that a doc.go holds no code.
func protected(file string) string {
	if !strings.HasPrefix(file, "core/") && !strings.HasPrefix(file, "sdk/") {
		return ""
	}
	if !strings.HasSuffix(file, ".go") || strings.HasSuffix(file, "_test.go") || strings.Contains(file, "/testdata/") || path.Base(file) == "doc.go" {
		return ""
	}
	return path.Dir(file)
}

// Check returns each rule of docs/SPEC-TEMPLATE.md that a pull request
// breaks:
//
//   - The record of the gates of a spec or a proposal only grows.
//   - The grandfathered lists only shrink.
//   - A change to a package under core/ or sdk/ names the spec that owns the
//     package, unless the package is on the grandfathered list of the base.
func Check(pr PullRequest) []string {
	var problems []string
	var packages []string
	listChanged := false
	for _, change := range pr.Changes {
		before := change.Path
		if change.Previous != "" {
			before = change.Previous
		}
		for _, file := range []string{change.Path, before} {
			if pkg := protected(file); pkg != "" && !slices.Contains(packages, pkg) {
				packages = append(packages, pkg)
			}
		}
		listChanged = listChanged || change.Path == grandfatheredPath
		if !isDocument(before) || change.Removed {
			// To delete a whole document is allowed: the history keeps its record.
			continue
		}
		old, ok := pr.Base(before)
		if !ok {
			continue
		}
		record, had := Record(old)
		if !had {
			continue
		}
		text, _ := pr.Head(change.Path)
		current, has := Record(text)
		if !has {
			problems = append(problems, fmt.Sprintf("%s: the section \"## Gates\" is gone. The record of the gates only grows", change.Path))
		} else if lost := AppendOnly(record, current); lost != "" {
			problems = append(problems, fmt.Sprintf("%s: the record of the gates lost or changed the line %q. Add a dated \"Revised on\" line; never edit the record", change.Path, lost))
		}
	}

	// exempt holds the packages that had no spec at the base.
	old, had := pr.Base(grandfatheredPath)
	oldSpecs, exempt := Grandfathered(old)
	_, rules := pr.Base(templatePath)
	text, ok := pr.Head(grandfatheredPath)
	if first := !had && !rules; first {
		// The pull request that adds the rules also adds the first list.
		// After it, only the list of the base counts.
		_, exempt = Grandfathered(text)
	} else if ok && listChanged {
		specs, unowned := Grandfathered(text)
		for _, entry := range slices.Concat(specs, unowned) {
			if !slices.Contains(oldSpecs, entry) && !slices.Contains(exempt, entry) {
				problems = append(problems, fmt.Sprintf("%s: the pull request adds %s. The list only shrinks", grandfatheredPath, entry))
			}
		}
	}

	var unowned []string
	for _, pkg := range packages {
		if !slices.Contains(exempt, pkg) {
			unowned = append(unowned, pkg)
		}
	}
	if len(unowned) == 0 {
		return problems
	}
	named := References(pr.Body)
	if len(named) == 0 {
		return append(problems, fmt.Sprintf("the pull request changes %s. Its description must name the spec of each package, in a line \"Spec: docs/<name>/SPEC.md\"", strings.Join(unowned, ", ")))
	}
	var owned []string
	for _, spec := range named {
		text, ok := pr.Head(spec)
		if !ok {
			problems = append(problems, fmt.Sprintf("the description names %s, which does not exist", spec))
		}
		owned = append(owned, Owns(text)...)
		// A pull request that removes a package finds its owner at the base.
		if old, ok := pr.Base(spec); ok {
			owned = append(owned, Owns(old)...)
		}
	}
	for _, pkg := range unowned {
		if !slices.Contains(owned, pkg) {
			problems = append(problems, fmt.Sprintf("the pull request changes %s, and no spec that its description names owns that package", pkg))
		}
	}
	return problems
}
