package specs

// These tests check the documents under docs/ against the rules of
// docs/SPEC-TEMPLATE.md: the header of each spec, the recorded answers to its
// gates, the proof that it names, the packages that it owns, and the registry
// of event kinds.

import (
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const root = "../.."

// The gates that a spec must answer, by the layers that it touches. The IDs
// never change, see docs/SPEC-TEMPLATE.md, section 3.
var (
	baseGates = []string{"A1", "A2", "A3", "B1", "B3", "B4", "D1", "D2", "D3", "D4"}
	sdkGates  = []string{"C1", "C2", "C3", "C4", "C5"}
	coreGates = []string{"B2", "D5"}
	allGates  = []string{"A1", "A2", "A3", "B1", "B2", "B3", "B4", "C1", "C2", "C3", "C4", "C5", "D1", "D2", "D3", "D4", "D5"}
	layers    = []string{"core", "sdk", "adapters", "runtime", "app"}
	// sections are the headings of the body of a spec, in their order. See
	// docs/SPEC-TEMPLATE.md, section 9.
	sections = []string{"1. Purpose", "2. Terms", "3. Rules", "4. Behavior", "5. Failures", "6. Security", "7. Compatibility", "8. Proof", "Gates"}
)

var (
	answeredBy  = regexp.MustCompile(`^Answered by .+ on \d{4}-\d{2}-\d{2} at [0-9a-f]{7,40}\.$`)
	placeholder = regexp.MustCompile(`(?i)^(n/?a|tbd|todo|none|yes|no|same as above|see above)[.!]?$`)
	testName    = regexp.MustCompile(`\bTest[A-Z]\w*`)
	commitHash  = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	link        = regexp.MustCompile(`https?://\S+`)
	remainsRef  = regexp.MustCompile(`(?i)section \d|https?://\S+|#\d+`)
	testFunc    = regexp.MustCompile(`(?m)^func (Test\w+)\(`)
)

// tree is what the checks need to know about the repository.
type tree struct {
	// tests maps a package directory to the names of its test functions.
	tests map[string][]string
	// packages holds each directory under core/ and sdk/ that has Go code.
	packages map[string]bool
	// exempt holds the specs that predate the gates.
	exempt map[string]bool
	// code holds each doc.go under core/ and sdk/ that holds a declaration.
	code []string
}

// required returns the gates that a spec with these layers must answer.
func required(touched []string) []string {
	gates := slices.Clone(baseGates)
	if slices.Contains(touched, "sdk") || slices.Contains(touched, "core") {
		gates = append(gates, sdkGates...)
	}
	if slices.Contains(touched, "core") {
		gates = append(gates, coreGates...)
	}
	sort.Strings(gates)
	return gates
}

// proves reports why a proof command names no test, or "".
func (t tree) proves(command string) string {
	fields := strings.Fields(command)
	if len(fields) < 3 || fields[0] != "go" || fields[1] != "test" {
		return "the proof must be one go test command"
	}
	pattern := ""
	var names []string
	for i := 2; i < len(fields); i++ {
		switch field := fields[i]; {
		case field == "-run" && i+1 < len(fields):
			pattern = strings.Trim(fields[i+1], `'"`)
			i++
		case strings.HasPrefix(field, "./"):
			dir := strings.Trim(strings.TrimPrefix(field, "./"), "/")
			for pkg, tests := range t.tests {
				if prefix, all := strings.CutSuffix(dir, "..."); all {
					if prefix = strings.TrimSuffix(prefix, "/"); prefix == "" || pkg == prefix || strings.HasPrefix(pkg, prefix+"/") {
						names = append(names, tests...)
					}
				} else if pkg == dir {
					names = append(names, tests...)
				}
			}
		}
	}
	run, err := regexp.Compile(pattern)
	if err != nil {
		return fmt.Sprintf("the -run pattern of the proof does not compile: %v", err)
	}
	if !slices.ContainsFunc(names, run.MatchString) {
		return fmt.Sprintf("the proof matches no test: -run %q finds nothing in the packages that it names", pattern)
	}
	return ""
}

// exists reports whether a test with this name is in the repository.
func (t tree) exists(name string) bool {
	for _, tests := range t.tests {
		if slices.Contains(tests, name) {
			return true
		}
	}
	return false
}

// checkAnswer returns what is wrong with the record of one gate.
func (t tree) checkAnswer(id string, lines []string) []string {
	var problems, quote []string
	attributed, evidence := false, ""
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, "> "):
			quote = append(quote, strings.TrimPrefix(line, "> "))
		case answeredBy.MatchString(line):
			attributed = true
		case strings.HasPrefix(line, "Evidence: "):
			evidence = strings.TrimSpace(strings.TrimPrefix(line, "Evidence: "))
		}
	}
	answer := strings.TrimSpace(strings.Join(quote, " "))
	switch {
	case answer == "":
		problems = append(problems, id+": no answer. Quote the answer of the person with \"> \"")
	case placeholder.MatchString(answer) || len(strings.Fields(answer)) < 8:
		problems = append(problems, fmt.Sprintf("%s: %q is not an answer. Give at least eight words", id, answer))
	case strings.Contains(answer, "in the words of the person who gave it"):
		problems = append(problems, id+": the answer is the text of the template")
	}
	if !attributed {
		problems = append(problems, id+`: no line "Answered by <name> on <YYYY-MM-DD> at <commit>."`)
	}
	named := testName.FindAllString(evidence, -1)
	anchored := commitHash.MatchString(evidence)
	switch {
	case evidence == "" || placeholder.MatchString(evidence):
		problems = append(problems, id+`: no line "Evidence: ..."`)
	case len(named) == 0 && !anchored && !link.MatchString(evidence):
		problems = append(problems, id+": the evidence must name a test, a link or a commit")
	}
	for _, name := range named {
		// A test that was removed is checkable only at a commit.
		if !t.exists(name) && !anchored {
			problems = append(problems, fmt.Sprintf("%s: the evidence names %s, which does not exist. Name the commit where it did", id, name))
		}
	}
	if id == "D1" && !slices.ContainsFunc(named, t.exists) {
		problems = append(problems, "D1: the evidence must name a test that exists")
	}
	return problems
}

// checkSpec returns what is wrong with one SPEC.md. path is the path of the
// spec from the root of the repository.
func (t tree) checkSpec(path, text string) []string {
	lines := visible(text)
	items, repeated := header(lines)
	var problems []string
	for _, name := range repeated {
		problems = append(problems, "the header has two items "+name)
	}
	missing := func(name string) bool {
		if items[name] == "" {
			problems = append(problems, "the header has no item "+name)
			return true
		}
		return false
	}

	status := items["Status"]
	switch status {
	case "built", "partial":
	case "proposed":
		problems = append(problems, "a proposed document lives under docs/proposals/, not in a SPEC.md")
	default:
		problems = append(problems, fmt.Sprintf("Status is %q; it must be partial or built", status))
	}

	touched := list(items["Layers"])
	if !missing("Layers") {
		for _, layer := range touched {
			if !slices.Contains(layers, layer) {
				problems = append(problems, fmt.Sprintf("Layers names %q; the layers are %s", layer, strings.Join(layers, ", ")))
			}
		}
		if len(touched) == 1 && touched[0] == "app" {
			problems = append(problems, "a spec that touches only app is refused: an app documents itself in its README")
		}
	}

	if !missing("Owns") && items["Owns"] != "none" {
		for _, pkg := range list(items["Owns"]) {
			layer, _, _ := strings.Cut(pkg, "/")
			switch {
			case !t.packages[pkg]:
				problems = append(problems, fmt.Sprintf("Owns names %s, which is not a package under core/ or sdk/", pkg))
			case !slices.Contains(touched, layer):
				problems = append(problems, fmt.Sprintf("the spec owns %s, so Layers must list %s", pkg, layer))
			}
		}
	}

	if !missing("Proof") {
		if why := t.proves(items["Proof"]); why != "" {
			problems = append(problems, why)
		}
	}
	if status == "partial" && !remainsRef.MatchString(items["Remaining"]) {
		problems = append(problems, "a partial spec needs an item Remaining that names a section or holds a link")
	}
	if !missing("Unverified") && (placeholder.MatchString(items["Unverified"]) || len(strings.Fields(items["Unverified"])) < 4) {
		problems = append(problems, fmt.Sprintf("Unverified is %q; say what no test covers", items["Unverified"]))
	}

	var headings []string
	for _, line := range lines {
		if heading, ok := strings.CutPrefix(line, "## "); ok {
			headings = append(headings, strings.TrimSpace(heading))
		}
	}
	if !slices.Equal(headings, sections) {
		problems = append(problems, fmt.Sprintf("the sections are %q; they must be %q, see docs/SPEC-TEMPLATE.md, section 9", headings, sections))
	}

	answers, found := gates(lines)
	for id := range answers {
		if !slices.Contains(allGates, id) {
			problems = append(problems, fmt.Sprintf("the Gates section has the unknown gate %s", id))
		}
	}
	if t.exempt[path] {
		if found && len(t.unanswered(touched, answers)) == 0 {
			problems = append(problems, "each gate has its answer: remove the spec from docs/GRANDFATHERED.md")
		}
		return problems
	}
	if !found {
		return append(problems, "the spec has no section \"## Gates\", see docs/SPEC-TEMPLATE.md")
	}
	return append(problems, t.unanswered(touched, answers)...)
}

// unanswered returns what is wrong with the required gates of a spec.
func (t tree) unanswered(touched []string, answers map[string][]string) []string {
	var problems []string
	for _, id := range required(touched) {
		lines, ok := answers[id]
		if !ok {
			problems = append(problems, id+": the gate has no answer")
			continue
		}
		problems = append(problems, t.checkAnswer(id, lines)...)
	}
	return problems
}

// load reads what the checks need from the repository.
func load(t *testing.T) tree {
	t.Helper()
	found := tree{tests: map[string][]string{}, packages: map[string]bool{}, exempt: map[string]bool{}}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if name := entry.Name(); name == "testdata" || (strings.HasPrefix(name, ".") && relative != ".") {
				return filepath.SkipDir
			}
			return nil
		}
		dir := filepath.ToSlash(filepath.Dir(relative))
		switch {
		case strings.HasSuffix(relative, "_test.go"):
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			for _, match := range testFunc.FindAllStringSubmatch(string(data), -1) {
				found.tests[dir] = append(found.tests[dir], match[1])
			}
		case strings.HasSuffix(relative, ".go") && (strings.HasPrefix(relative, "core/") || strings.HasPrefix(relative, "sdk/")):
			// A doc.go is documentation. It makes no package that needs a
			// spec, so it must hold no code.
			if entry.Name() != "doc.go" {
				found.packages[dir] = true
				break
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				return err
			}
			if len(file.Decls) != 0 {
				found.code = append(found.code, relative)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	specs, _ := grandfathered(t)
	for _, spec := range specs {
		found.exempt[spec] = true
	}
	return found
}

// grandfathered reads docs/GRANDFATHERED.md: the specs that predate the
// gates, and the packages that no spec owns.
func grandfathered(t *testing.T) (specs, packages []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "docs", "GRANDFATHERED.md"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return Grandfathered(string(data))
}

// documents returns the path and the text of each Markdown file that match
// under docs/.
func documents(t *testing.T, match func(path string) bool) map[string]string {
	t.Helper()
	found := map[string]string{}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		if relative = filepath.ToSlash(relative); match(relative) {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			found[relative] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// Each spec has its header, a proof that names a test, and the record of its
// gates. A spec that predates the gates is in docs/GRANDFATHERED.md.
func TestEachSpecFollowsTheTemplate(t *testing.T) {
	found := load(t)
	specs := documents(t, isSpec)
	if len(specs) == 0 {
		t.Fatal("found no SPEC.md under docs/")
	}
	for path, text := range specs {
		for _, problem := range found.checkSpec(path, text) {
			t.Errorf("%s: %s", path, problem)
		}
	}
}

// A proposal says that it is one, so nobody reads it as built.
func TestEachProposalSaysProposed(t *testing.T) {
	proposals := documents(t, func(path string) bool { return strings.HasPrefix(path, "docs/proposals/") })
	for path, text := range proposals {
		said := false
		for _, line := range visible(text) {
			if strings.HasPrefix(line, "## ") {
				break
			}
			said = said || strings.HasPrefix(strings.TrimPrefix(line, "- "), "Status: proposed")
		}
		if !said {
			t.Errorf("%s: a proposal starts with a line \"Status: proposed\"", path)
		}
	}
}

// Each package under core/ and sdk/ belongs to a spec, so no code enters the
// protected layers without one.
func TestEachCoreAndSDKPackageHasASpec(t *testing.T) {
	found := load(t)
	owners := map[string][]string{}
	for path, text := range documents(t, isSpec) {
		for _, pkg := range Owns(text) {
			owners[pkg] = append(owners[pkg], path)
		}
	}
	for _, file := range found.code {
		t.Errorf("%s: a doc.go holds only the package comment. Move its code to another file", file)
	}
	_, unowned := grandfathered(t)
	for pkg := range found.packages {
		switch listed := slices.Contains(unowned, pkg); {
		case len(owners[pkg]) == 0 && !listed:
			t.Errorf("%s: no spec owns the package. Name it in the Owns item of a spec", pkg)
		case len(owners[pkg]) > 0 && listed:
			t.Errorf("%s: %s owns the package: remove it from docs/GRANDFATHERED.md", pkg, owners[pkg][0])
		}
	}
	for _, pkg := range unowned {
		if !found.packages[pkg] {
			t.Errorf("docs/GRANDFATHERED.md lists %s, which is not a package under core/ or sdk/", pkg)
		}
	}
	specs, _ := grandfathered(t)
	for _, spec := range specs {
		if _, err := os.Stat(filepath.Join(root, spec)); err != nil {
			t.Errorf("docs/GRANDFATHERED.md lists %s, which does not exist", spec)
		}
	}
}

// class gives the NIP-01 class of a kind.
func class(kind int) string {
	switch {
	case kind == 0 || kind == 3 || (kind >= 10000 && kind < 20000):
		return "replaceable"
	case kind >= 20000 && kind < 30000:
		return "ephemeral"
	case kind >= 30000 && kind < 40000:
		return "addressable"
	}
	return "regular"
}

var (
	// A constant whose name holds Kind, with a literal number. A kind that
	// the code names through a constant of the Nostr library is not found.
	kindConstant = regexp.MustCompile(`(?m)^\s*(?:const\s+)?\w*Kind\w*\s+(?:nostr\.Kind\s+)?=\s+(\d+)\b`)
	reservedMap  = regexp.MustCompile(`(?s)var reserved = map\[int\]string\{(.*?)\n\}`)
	reservedKey  = regexp.MustCompile(`(\d+):`)
)

// kindsInUse reads the kinds that the code, the manifests of the apps, and
// the reserved list name.
func kindsInUse(t *testing.T) (code, manifest, reserved map[int]string) {
	t.Helper()
	code, manifest, reserved = map[int]string{}, map[int]string{}, map[int]string{}
	for _, dir := range []string{"core", "runtime", "sdk", "adapters", "cmd", "apps"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			relative, _ := filepath.Rel(root, path)
			relative = filepath.ToSlash(relative)
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			switch {
			case strings.HasSuffix(relative, ".go") && !strings.HasSuffix(relative, "_test.go"):
				for _, match := range kindConstant.FindAllStringSubmatch(string(data), -1) {
					kind, _ := strconv.Atoi(match[1])
					code[kind] = relative
				}
			case dir == "apps" && entry.Name() == "manifest.json":
				var held struct {
					Kinds map[string]struct {
						Kind int `json:"kind"`
					} `json:"kinds"`
				}
				if err := json.Unmarshal(data, &held); err != nil {
					return fmt.Errorf("%s: %w", relative, err)
				}
				for _, kind := range held.Kinds {
					manifest[kind.Kind] = relative
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	const consent = "runtime/iface/consent.go"
	data, err := os.ReadFile(filepath.Join(root, consent))
	if err != nil {
		t.Fatal(err)
	}
	block := reservedMap.FindStringSubmatch(string(data))
	if block == nil {
		t.Fatalf("%s holds no reserved list", consent)
	}
	for _, match := range reservedKey.FindAllStringSubmatch(block[1], -1) {
		kind, _ := strconv.Atoi(match[1])
		reserved[kind] = consent
	}
	return code, manifest, reserved
}

// The registry docs/KINDS.md and the code agree: no kind is in use without a
// row, and no row names a kind that nothing uses.
func TestTheKindRegistryMatchesTheCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(root, "docs", "KINDS.md"))
	if err != nil {
		t.Fatal(err)
	}
	type row struct{ class, source, spec string }
	rows := map[int]row{}
	last := -1
	for _, line := range visible(string(data)) {
		cells := strings.Split(line, "|")
		if len(cells) != 8 {
			continue
		}
		kind, err := strconv.Atoi(strings.TrimSpace(cells[1]))
		if err != nil {
			continue
		}
		if kind <= last {
			t.Errorf("kind %d: the rows must rise, with each kind once", kind)
		}
		last = kind
		rows[kind] = row{strings.TrimSpace(cells[2]), strings.TrimSpace(cells[5]), strings.TrimSpace(cells[6])}
	}
	if len(rows) == 0 {
		t.Fatal("docs/KINDS.md holds no row")
	}

	code, manifest, reserved := kindsInUse(t)
	target := regexp.MustCompile(`\]\(([^)#]+)`)
	for kind, held := range rows {
		if want := class(kind); held.class != want {
			t.Errorf("kind %d: the class is %q; NIP-01 gives %q", kind, held.class, want)
		}
		sources := list(held.source)
		for _, source := range sources {
			in, ok := map[string]map[int]string{"code": code, "manifest": manifest, "reserved": reserved}[source]
			switch {
			case !ok:
				t.Errorf("kind %d: the source is %q; it must be code, manifest or reserved", kind, source)
			case in[kind] == "":
				t.Errorf("kind %d: the registry says %s, but no %s names the kind", kind, source, source)
			}
		}
		if len(sources) == 0 {
			t.Errorf("kind %d: the row has no source", kind)
		}
		match := target.FindStringSubmatch(held.spec)
		if match == nil {
			t.Errorf("kind %d: the row links no spec", kind)
		} else if _, err := os.Stat(filepath.Join(root, "docs", match[1])); err != nil {
			t.Errorf("kind %d: the row links %s, which does not exist", kind, match[1])
		}
	}
	for name, in := range map[string]map[int]string{"code": code, "manifest": manifest} {
		for kind, where := range in {
			if held, ok := rows[kind]; !ok {
				t.Errorf("kind %d: %s names it, but docs/KINDS.md has no row", kind, where)
			} else if !slices.Contains(list(held.source), name) {
				t.Errorf("kind %d: %s names it, so the source of its row must list %s", kind, where, name)
			}
		}
	}
	for kind, where := range reserved {
		if _, ok := rows[kind]; !ok {
			t.Errorf("kind %d: %s reserves it, but docs/KINDS.md has no row", kind, where)
		}
	}
}
