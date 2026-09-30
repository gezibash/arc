package specs

import (
	"slices"
	"strings"
	"testing"
)

// files serves the files of one side of a pull request.
func files(held map[string]string) func(string) (string, bool) {
	return func(path string) (string, bool) {
		text, ok := held[path]
		return text, ok
	}
}

const (
	deliverySpec = "# Delivery\n\n- Status: built\n- Layers: core\n- Owns: core/keys, core/old\n"
	httpSpec     = "# HTTP\n\n- Status: built\n- Layers: sdk\n- Owns: sdk/httpadapter\n"
	debtList     = "# Debt\n\n## Specs without gates\n\n| Spec | Since |\n| --- | --- |\n| `docs/old/SPEC.md` | 2026-09-30 |\n\n" +
		"## Packages without a spec\n\n| Package | Since |\n| --- | --- |\n| `sdk/strictjson` | 2026-09-30 |\n"
)

// gated is a spec with a record of two gates.
func gated(a1, extra string) string {
	return "# A spec\n\n- Status: built\n\n## 1. Purpose\n\nText.\n\n## Gates\n\n" +
		"### A1. What fails today without this?\n\n> " + a1 + "\n\nAnswered by Ada Lovelace on 2026-09-30 at 25c46fc.\nEvidence: TestProof\n" + extra +
		"\n### A2. Can it be an app?\n\n> No, because the relay must verify the seal itself.\n\nAnswered by Ada Lovelace on 2026-09-30 at 25c46fc.\nEvidence: TestOther\n"
}

const a1 = "The relay accepts a wrap that holds no seal."

func TestAPullRequestNamesTheSpecOfEachProtectedPackage(t *testing.T) {
	base := map[string]string{"docs/delivery/SPEC.md": deliverySpec, "docs/http/SPEC.md": httpSpec, grandfatheredPath: debtList, templatePath: "rules"}
	for name, held := range map[string]struct {
		body    string
		changes []Change
		head    map[string]string
		want    string
	}{
		"named":             {"Spec: docs/delivery/SPEC.md", []Change{{Path: "core/keys/keys.go"}}, base, ""},
		"named after prose": {"Fixes the seal check.\r\n\r\nSpec: docs/delivery/SPEC.md, section 5\r\n", []Change{{Path: "core/keys/keys.go"}}, base, ""},
		"not named":         {"Fixes the seal check.", []Change{{Path: "core/keys/keys.go"}}, base, `changes core/keys. Its description must name the spec`},
		"wrong spec":        {"Spec: docs/http/SPEC.md", []Change{{Path: "core/keys/keys.go"}}, base, "changes core/keys, and no spec that its description names owns that package"},
		"missing spec":      {"Spec: docs/nothing/SPEC.md", []Change{{Path: "core/keys/keys.go"}}, base, "names docs/nothing/SPEC.md, which does not exist"},
		"new package":       {"Spec: docs/delivery/SPEC.md", []Change{{Path: "core/fresh/fresh.go"}}, base, "changes core/fresh, and no spec"},
		"grandfathered":     {"", []Change{{Path: "sdk/strictjson/strictjson.go"}}, base, ""},
		"tests only":        {"", []Change{{Path: "core/keys/keys_test.go"}, {Path: "core/keys/testdata/vector.json"}, {Path: "core/keys/README.md"}}, base, ""},
		"other layers":      {"", []Change{{Path: "runtime/iface/run.go"}, {Path: "README.md"}}, base, ""},
		// A doc.go is documentation. Code in the root of a layer is not.
		"package comment":   {"", []Change{{Path: "core/doc.go"}, {Path: "core/keys/doc.go"}}, base, ""},
		"code in the root":  {"", []Change{{Path: "core/extra.go"}}, base, "changes core. Its description must name the spec"},
		"moved out of core": {"", []Change{{Path: "runtime/keys/keys.go", Previous: "core/keys/keys.go"}}, base, "changes core/keys"},
		// The spec stops owning a package that the pull request removes.
		"removed package": {"Spec: docs/delivery/SPEC.md", []Change{{Path: "core/old/old.go", Removed: true}},
			map[string]string{"docs/delivery/SPEC.md": "# Delivery\n\n- Status: built\n- Layers: core\n- Owns: core/keys\n", grandfatheredPath: debtList}, ""},
	} {
		problems := Check(PullRequest{Body: held.body, Changes: held.changes, Base: files(base), Head: files(held.head)})
		expect(t, name, problems, held.want)
	}
}

func TestTheRecordOfTheGatesOnlyGrows(t *testing.T) {
	const spec = "docs/a/SPEC.md"
	base := map[string]string{spec: gated(a1, ""), "docs/proposals/a.md": gated(a1, ""), "docs/plain/SPEC.md": "# Plain\n\n## 1. Purpose\n"}
	reordered := strings.Replace(gated(a1, ""), "### A1. What fails today without this?", "### A9. Moved", 1)
	for name, held := range map[string]struct {
		change Change
		head   string
		want   string
	}{
		"revised":         {Change{Path: spec}, gated(a1, "Revised on 2026-10-04 by Ada Lovelace at 9f1e2c3: the relay also logs it.\n"), ""},
		"new gate":        {Change{Path: spec}, gated(a1, "") + "\n### B1. Which layers?\n\n> Core, because each relay must agree on the seal.\n", ""},
		"design changed":  {Change{Path: spec}, strings.Replace(gated(a1, ""), "Text.", "Better text.", 1), ""},
		"answer edited":   {Change{Path: spec}, gated("The relay is fine.", ""), `lost or changed the line "> ` + a1},
		"evidence gone":   {Change{Path: spec}, strings.Replace(gated(a1, ""), "Evidence: TestProof\n", "", 1), `lost or changed the line "Evidence: TestProof"`},
		"heading changed": {Change{Path: spec}, reordered, `lost or changed the line "### A1.`},
		"section gone":    {Change{Path: spec}, "# A spec\n\n## 1. Purpose\n\nText.\n", `the section "## Gates" is gone`},
		"spec deleted":    {Change{Path: spec, Removed: true}, "", ""},
		"renamed":         {Change{Path: "docs/b/SPEC.md", Previous: spec}, gated(a1, ""), ""},
		"renamed, edited": {Change{Path: "docs/b/SPEC.md", Previous: spec}, gated("The relay is fine.", ""), "lost or changed the line"},
		"first record":    {Change{Path: "docs/plain/SPEC.md"}, gated(a1, ""), ""},
		// A proposal holds a record too, and keeps it when it becomes a spec.
		"proposal edited": {Change{Path: "docs/proposals/a.md"}, gated("The relay is fine.", ""), "lost or changed the line"},
		"promoted":        {Change{Path: "docs/a2/SPEC.md", Previous: "docs/proposals/a.md"}, gated(a1, ""), ""},
	} {
		head := map[string]string{}
		if !held.change.Removed {
			head[held.change.Path] = held.head
		}
		problems := Check(PullRequest{Changes: []Change{held.change}, Base: files(base), Head: files(head)})
		expect(t, name, problems, held.want)
	}
}

func TestTheGrandfatheredListOnlyShrinks(t *testing.T) {
	changed := []Change{{Path: grandfatheredPath}}
	withRules := map[string]string{grandfatheredPath: debtList, templatePath: "rules"}
	for name, held := range map[string]struct {
		base map[string]string
		head string
		want string
	}{
		"row removed":   {withRules, strings.Replace(debtList, "| `sdk/strictjson` | 2026-09-30 |\n", "", 1), ""},
		"package added": {withRules, debtList + "| `sdk/fresh` | 2026-10-04 |\n", "adds sdk/fresh. The list only shrinks"},
		"spec added":    {withRules, strings.Replace(debtList, "| `docs/old/SPEC.md` | 2026-09-30 |\n", "| `docs/old/SPEC.md` | 2026-09-30 |\n| `docs/new/SPEC.md` | 2026-10-04 |\n", 1), "adds docs/new/SPEC.md. The list only shrinks"},
		// The pull request that adds the rules also adds the first list.
		"first list":     {map[string]string{}, debtList, ""},
		"list came back": {map[string]string{templatePath: "rules"}, debtList, "adds docs/old/SPEC.md. The list only shrinks"},
	} {
		problems := Check(PullRequest{Changes: changed, Base: files(held.base), Head: files(map[string]string{grandfatheredPath: held.head})})
		expect(t, name, problems, held.want)
	}
}

// expect checks that a pull request has no problem, or the one that want
// names.
func expect(t *testing.T, name string, problems []string, want string) {
	t.Helper()
	switch {
	case want == "" && len(problems) != 0:
		t.Errorf("%s: want no problem, got %q", name, problems)
	case want != "" && !slices.ContainsFunc(problems, func(problem string) bool { return strings.Contains(problem, want) }):
		t.Errorf("%s: want a problem with %q, got %q", name, want, problems)
	}
}

func TestAppendOnly(t *testing.T) {
	for name, held := range map[string]struct{ base, head, want string }{
		"same":      {"a b c", "a b c", ""},
		"added":     {"a b c", "a x b c y", ""},
		"removed":   {"a b c", "a c", "b"},
		"reordered": {"a b c", "a c b", "c"},
		"empty":     {"a", "", "a"},
		// The same line twice must stay twice.
		"repeat lost": {"a a", "a", "a"},
	} {
		if got := AppendOnly(strings.Fields(held.base), strings.Fields(held.head)); got != held.want {
			t.Errorf("%s: lost %q, want %q", name, got, held.want)
		}
	}
}

// The pull request that adds the rules has no list at its base. Its own list
// exempts its packages. After that, only the list of the base counts.
func TestOnlyTheFirstPullRequestTrustsItsOwnList(t *testing.T) {
	changes := []Change{{Path: "sdk/strictjson/strictjson.go"}, {Path: grandfatheredPath}}
	head := files(map[string]string{grandfatheredPath: debtList})

	problems := Check(PullRequest{Changes: changes, Base: files(map[string]string{}), Head: head})
	expect(t, "first list", problems, "")

	problems = Check(PullRequest{Changes: changes, Base: files(map[string]string{templatePath: "rules"}), Head: head})
	expect(t, "list of the head, with rules at the base", problems, "changes sdk/strictjson. Its description must name the spec")
}
