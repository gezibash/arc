package specs

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// fixture is a small repository: one package with two tests, and two
// protected packages.
var fixture = tree{
	tests:    map[string][]string{"pkg/a": {"TestProof", "TestOther"}},
	packages: map[string]bool{"core/keys": true, "sdk/provider": true},
	exempt:   map[string]bool{},
}

const goodAnswer = "The relay refuses a wrap that holds no seal."

// record writes the record of one gate, as docs/SPEC-TEMPLATE.md, section 4.
func record(id, answer, attribution, evidence string) string {
	return fmt.Sprintf("### %s. A question?\n\n> %s\n\n%s\n%s\n\n", id, answer, attribution, evidence)
}

// body is the body of a spec, before its Gates section.
const body = "## 1. Purpose\n\nText.\n\n## 2. Terms\n\n## 3. Rules\n\n## 4. Behavior\n\n## 5. Failures\n\n" +
	"## 6. Security\n\n## 7. Compatibility\n\n## 8. Proof\n\n"

// document writes a spec with a header, a body, and one good record for each
// gate in ids. change replaces the record of single gates.
func document(head string, ids []string, change map[string]string) string {
	var out strings.Builder
	out.WriteString("# A spec\n\n" + head + "\n\n" + body + "## Gates\n\n")
	for _, id := range ids {
		if changed, ok := change[id]; ok {
			out.WriteString(changed)
			continue
		}
		out.WriteString(record(id, goodAnswer, "Answered by Ada Lovelace on 2026-09-30 at 25c46fc.", "Evidence: TestProof fails without it."))
	}
	return out.String()
}

const runtimeHead = `- Status: built
- Layers: runtime
- Owns: none
- Proof: go test -count=1 -run '^TestProof$' ./pkg/a/
- Unverified: No test uses a public relay.`

func head(replace, with string) string { return strings.Replace(runtimeHead, replace, with, 1) }

const by = "Answered by Ada Lovelace on 2026-09-30 at 25c46fc."

func TestASpecThatFollowsTheTemplateHasNoProblem(t *testing.T) {
	for name, text := range map[string]string{
		"runtime": document(runtimeHead, baseGates, nil),
		"sdk":     document(head("Layers: runtime", "Layers: sdk"), slices.Concat(baseGates, sdkGates), nil),
		"core":    document(head("Layers: runtime", "Layers: core"), allGates, nil),
		"owner":   document(head("Layers: runtime\n- Owns: none", "Layers: core\n- Owns: core/keys"), allGates, nil),
		"partial": document(head("Status: built", "Status: partial\n- Remaining: phase 4 of section 18"), baseGates, nil),
		// Eight words are the least that count as an answer.
		"eight words": document(runtimeHead, baseGates, map[string]string{"A1": record("A1", "one two three four five six seven eight", by, "Evidence: TestProof")}),
		// A removed test is evidence when the line names the commit.
		"removed test": document(runtimeHead, baseGates, map[string]string{"A1": record("A1", goodAnswer, by, "Evidence: TestGone failed at 25c46fc.")}),
		"link":         document(runtimeHead, baseGates, map[string]string{"A1": record("A1", goodAnswer, by, "Evidence: https://example.com/issues/7")}),
	} {
		if problems := fixture.checkSpec("docs/a/SPEC.md", text); len(problems) != 0 {
			t.Errorf("%s: a correct spec has problems: %q", name, problems)
		}
	}
}

func TestASpecThatBreaksARuleHasThatProblem(t *testing.T) {
	for name, held := range map[string]struct{ text, want string }{
		"placeholder answer": {document(runtimeHead, baseGates, map[string]string{"A1": record("A1", "N/A", by, "Evidence: TestProof")}), `A1: "N/A" is not an answer`},
		"seven words":        {document(runtimeHead, baseGates, map[string]string{"A2": record("A2", "one two three four five six seven", by, "Evidence: TestProof")}), "A2: \"one two three four five six seven\" is not an answer"},
		"template text":      {document(runtimeHead, baseGates, map[string]string{"A3": record("A3", "The answer, in the words of the person who gave it.", by, "Evidence: TestProof")}), "A3: the answer is the text of the template"},
		"no quote":           {document(runtimeHead, baseGates, map[string]string{"B1": "### B1. A question?\n\n" + goodAnswer + "\n\n" + by + "\nEvidence: TestProof\n\n"}), "B1: no answer"},
		"no attribution":     {document(runtimeHead, baseGates, map[string]string{"B3": record("B3", goodAnswer, "", "Evidence: TestProof")}), `B3: no line "Answered by`},
		"no commit":          {document(runtimeHead, baseGates, map[string]string{"B3": record("B3", goodAnswer, "Answered by Ada Lovelace on 2026-09-30.", "Evidence: TestProof")}), `B3: no line "Answered by`},
		"no evidence":        {document(runtimeHead, baseGates, map[string]string{"B4": record("B4", goodAnswer, by, "")}), `B4: no line "Evidence`},
		"vague evidence":     {document(runtimeHead, baseGates, map[string]string{"D2": record("D2", goodAnswer, by, "Evidence: it works on my machine")}), "D2: the evidence must name a test, a link or a commit"},
		"missing test":       {document(runtimeHead, baseGates, map[string]string{"D3": record("D3", goodAnswer, by, "Evidence: TestGone covers it")}), "D3: the evidence names TestGone, which does not exist"},
		"proof by link":      {document(runtimeHead, baseGates, map[string]string{"D1": record("D1", goodAnswer, by, "Evidence: https://example.com/run/7")}), "D1: the evidence must name a test that exists"},
		"missing gate":       {document(runtimeHead, baseGates[1:], nil), "A1: the gate has no answer"},
		"unknown gate":       {document(runtimeHead, append(slices.Clone(baseGates), "Z9"), nil), "unknown gate Z9"},
		"no gates":           {"# A spec\n\n" + runtimeHead + "\n\n" + body, `no section "## Gates"`},
		"missing section":    {strings.Replace(document(runtimeHead, baseGates, nil), "## 6. Security\n\n", "", 1), "they must be"},
		"sections reordered": {strings.Replace(document(runtimeHead, baseGates, nil), "## 2. Terms\n\n## 3. Rules", "## 3. Rules\n\n## 2. Terms", 1), "they must be"},
		"extra section":      {strings.Replace(document(runtimeHead, baseGates, nil), "## 8. Proof", "## 8. Proof\n\n## 9. History", 1), "they must be"},
		"renamed section":    {strings.Replace(document(runtimeHead, baseGates, nil), "## 3. Rules", "## 3. The rules", 1), "they must be"},
		"sdk skips cost":     {document(head("Layers: runtime", "Layers: sdk"), baseGates, nil), "C3: the gate has no answer"},
		"core skips wire":    {document(head("Layers: runtime", "Layers: core"), slices.Concat(baseGates, sdkGates), nil), "B2: the gate has no answer"},
		"core skips oracle":  {document(head("Layers: runtime", "Layers: core"), slices.Concat(baseGates, sdkGates), nil), "D5: the gate has no answer"},
		// A spec cannot declare a lower layer to skip the gates of core.
		"owns core, says runtime": {document(head("Owns: none", "Owns: core/keys"), baseGates, nil), "the spec owns core/keys, so Layers must list core"},
		"owns nothing real":       {document(head("Owns: none", "Owns: core/nothing"), baseGates, nil), "Owns names core/nothing, which is not a package"},
		"proof names no test":     {document(head("TestProof$", "TestNope$"), baseGates, nil), "the proof matches no test"},
		"proof is not go test":    {document(head("go test -count=1 -run '^TestProof$' ./pkg/a/", "make test"), baseGates, nil), "the proof must be one go test command"},
		"proposed":                {document(head("Status: built", "Status: proposed"), baseGates, nil), "a proposed document lives under docs/proposals/"},
		"no status":               {document(head("- Status: built\n", ""), baseGates, nil), `Status is ""`},
		"status in an example":    {document(head("- Status: built", "```\n- Status: built\n```"), baseGates, nil), `Status is ""`},
		"two statuses":            {document(head("- Status: built", "- Status: built\n- Status: partial"), baseGates, nil), "the header has two items Status"},
		"open partial":            {document(head("Status: built", "Status: partial"), baseGates, nil), "a partial spec needs an item Remaining"},
		"vague partial":           {document(head("Status: built", "Status: partial\n- Remaining: some work"), baseGates, nil), "a partial spec needs an item Remaining"},
		"app only":                {document(head("Layers: runtime", "Layers: app"), baseGates, nil), "a spec that touches only app is refused"},
		"unknown layer":           {document(head("Layers: runtime", "Layers: kernel"), baseGates, nil), `Layers names "kernel"`},
		"nothing unverified":      {document(head("Unverified: No test uses a public relay.", "Unverified: none"), baseGates, nil), `Unverified is "none"`},
	} {
		problems := fixture.checkSpec("docs/a/SPEC.md", held.text)
		if !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, held.want) }) {
			t.Errorf("%s: want a problem with %q, got %q", name, held.want, problems)
		}
	}
}

// A spec on the grandfathered list needs no gates. When it has them all, the
// list must let it go.
func TestAGrandfatheredSpec(t *testing.T) {
	exempt := fixture
	exempt.exempt = map[string]bool{"docs/a/SPEC.md": true}

	if problems := exempt.checkSpec("docs/a/SPEC.md", "# A spec\n\n"+runtimeHead+"\n\n"+body+"## Gates\n"); len(problems) != 0 {
		t.Errorf("a grandfathered spec with no gates has problems: %q", problems)
	}
	problems := exempt.checkSpec("docs/a/SPEC.md", document(runtimeHead, baseGates, nil))
	if len(problems) != 1 || !strings.Contains(problems[0], "remove the spec from docs/GRANDFATHERED.md") {
		t.Errorf("a grandfathered spec with each gate answered: got %q", problems)
	}
	// The header rules hold for a grandfathered spec too.
	problems = exempt.checkSpec("docs/a/SPEC.md", "# A spec\n\n"+head("TestProof$", "TestNope$")+"\n\n"+body+"## Gates\n")
	if len(problems) != 1 || !strings.Contains(problems[0], "the proof matches no test") {
		t.Errorf("a grandfathered spec with a false proof: got %q", problems)
	}
}

// The gates of each tier, as the table in docs/SPEC-TEMPLATE.md, section 3.
func TestRequiredGatesByLayer(t *testing.T) {
	for touched, want := range map[string]string{
		"runtime":           "A1 A2 A3 B1 B3 B4 D1 D2 D3 D4",
		"adapters, runtime": "A1 A2 A3 B1 B3 B4 D1 D2 D3 D4",
		"sdk, app":          "A1 A2 A3 B1 B3 B4 C1 C2 C3 C4 C5 D1 D2 D3 D4",
		"core":              "A1 A2 A3 B1 B2 B3 B4 C1 C2 C3 C4 C5 D1 D2 D3 D4 D5",
		"core, adapters":    "A1 A2 A3 B1 B2 B3 B4 C1 C2 C3 C4 C5 D1 D2 D3 D4 D5",
	} {
		if got := strings.Join(required(list(touched)), " "); got != want {
			t.Errorf("layers %q: gates %q, want %q", touched, got, want)
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
