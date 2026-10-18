package specs

import (
	"slices"
	"strings"
	"testing"
)

// fixture is a small repository: one package with two tests, and two
// protected packages.
var fixture = tree{
	tests:    map[string][]string{"pkg/a": {"TestProof", "TestOther"}},
	packages: map[string]bool{"core/keys": true, "sdk/provider": true},
}

// body is the body of a spec, after its header.
const body = "## 1. Purpose\n\nText.\n\n## 2. Terms\n\n## 3. Rules\n\n## 4. Behavior\n\n## 5. Failures\n\n" +
	"## 6. Security\n\n## 7. Compatibility\n\n## 8. Proof\n"

// document writes a spec with a header and a body.
func document(head string) string {
	return "# A spec\n\n" + head + "\n\n" + body
}

const runtimeHead = `- Status: built
- Layers: runtime
- Owns: none
- Proof: go test -count=1 -run '^TestProof$' ./pkg/a/
- Unverified: No test uses a public relay.`

func head(replace, with string) string { return strings.Replace(runtimeHead, replace, with, 1) }

func TestASpecThatFollowsTheTemplateHasNoProblem(t *testing.T) {
	for name, text := range map[string]string{
		"runtime": document(runtimeHead),
		"sdk":     document(head("Layers: runtime", "Layers: sdk")),
		"core":    document(head("Layers: runtime", "Layers: core")),
		"owner":   document(head("Layers: runtime\n- Owns: none", "Layers: core\n- Owns: core/keys")),
		"partial": document(head("Status: built", "Status: partial\n- Remaining: phase 4 of section 18")),
	} {
		if problems := fixture.checkSpec("docs/a/SPEC.md", text); len(problems) != 0 {
			t.Errorf("%s: a correct spec has problems: %q", name, problems)
		}
	}
}

func TestASpecThatBreaksARuleHasThatProblem(t *testing.T) {
	for name, held := range map[string]struct{ text, want string }{
		"missing section":         {strings.Replace(document(runtimeHead), "## 6. Security\n\n", "", 1), "they must be"},
		"sections reordered":      {strings.Replace(document(runtimeHead), "## 2. Terms\n\n## 3. Rules", "## 3. Rules\n\n## 2. Terms", 1), "they must be"},
		"extra section":           {strings.Replace(document(runtimeHead), "## 8. Proof", "## 8. Proof\n\n## 9. History", 1), "they must be"},
		"renamed section":         {strings.Replace(document(runtimeHead), "## 3. Rules", "## 3. The rules", 1), "they must be"},
		"owns core, says runtime": {document(head("Owns: none", "Owns: core/keys")), "the spec owns core/keys, so Layers must list core"},
		"owns nothing real":       {document(head("Owns: none", "Owns: core/nothing")), "Owns names core/nothing, which is not a package"},
		"proof names no test":     {document(head("TestProof$", "TestNope$")), "the proof matches no test"},
		"proof names a lost test": {document(head("^TestProof$", "^(TestProof|TestGone)$")), "the proof names TestGone, which is no test"},
		"proof is not go test":    {document(head("go test -count=1 -run '^TestProof$' ./pkg/a/", "make test")), "the proof must be one go test command"},
		"proposed":                {document(head("Status: built", "Status: proposed")), "a proposed document lives under docs/proposals/"},
		"no status":               {document(head("- Status: built\n", "")), `Status is ""`},
		"status in an example":    {document(head("- Status: built", "```\n- Status: built\n```")), `Status is ""`},
		"two statuses":            {document(head("- Status: built", "- Status: built\n- Status: partial")), "the header has two items Status"},
		"open partial":            {document(head("Status: built", "Status: partial")), "a partial spec needs an item Remaining"},
		"vague partial":           {document(head("Status: built", "Status: partial\n- Remaining: some work")), "a partial spec needs an item Remaining"},
		"app only":                {document(head("Layers: runtime", "Layers: app")), "a spec that touches only app is refused"},
		"unknown layer":           {document(head("Layers: runtime", "Layers: kernel")), `Layers names "kernel"`},
		"nothing unverified":      {document(head("Unverified: No test uses a public relay.", "Unverified: none")), `Unverified is "none"`},
	} {
		problems := fixture.checkSpec("docs/a/SPEC.md", held.text)
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, held.want) }) {
			t.Errorf("%s: want a problem with %q, got %q", name, held.want, problems)
		}
	}
}

// The classes of NIP-01, at each boundary.
func TestKindClass(t *testing.T) {
	for kind, want := range map[int]string{
		0: "replaceable", 1: "regular", 3: "replaceable", 4: "regular", 62: "regular", 9999: "regular",
		10000: "replaceable", 19999: "replaceable",
		20000: "ephemeral", 29999: "ephemeral",
		30000: "addressable", 39999: "addressable",
		40000: "regular",
	} {
		if got := class(kind); got != want {
			t.Errorf("kind %d: class %q, want %q", kind, got, want)
		}
	}
}

// The scan finds a kind that is a literal constant, in each form that the
// code uses, and nothing else.
func TestKindConstantScan(t *testing.T) {
	source := `
const (
	Kind           nostr.Kind = 31234
	CheckpointKind nostr.Kind = 1234
)
const SessionKind nostr.Kind = 3276
const KeyedRootKind = 30078
	event.Kind = nostr.Kind(kind)
	kind := 5
	limit = 4096
`
	var got []string
	for _, match := range kindConstant.FindAllStringSubmatch(source, -1) {
		got = append(got, match[1])
	}
	if want := "31234 1234 3276 30078"; strings.Join(got, " ") != want {
		t.Errorf("the scan found %q, want %q", strings.Join(got, " "), want)
	}
}
