package capability_test

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/go/capability"
	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/internal/vectors"
)

// The Elixir implementation signed these packages. Go reads the same files,
// and must reach the same hash and the same signature. This pins the
// normalizer of both implementations, and the canonical encoder as well.
func TestSignsTheSamePackageAsElixir(t *testing.T) {
	for _, want := range vectors.Load(t).Packages {
		signer, err := identity.FromSeedHex(want.SignerSeed)
		if err != nil {
			t.Fatal(err)
		}

		pkg, err := capability.LoadFile(filepath.Join("..", "..", want.Path))
		if err != nil {
			t.Fatalf("%s: %v", want.Path, err)
		}

		signed, err := capability.Sign(signer, pkg)
		if err != nil {
			t.Fatalf("%s: %v", want.Path, err)
		}

		if signed["package_hash"] != want.PackageHash {
			t.Errorf("%s: hash = %v, want %s", want.Path, signed["package_hash"], want.PackageHash)
		}

		signature, _ := signed["signature"].(map[string]any)
		if signature["value"] != want.Signature {
			t.Errorf("%s: the signature differs", want.Path)
		}
	}
}

// A package that Elixir signed verifies in Go.
func TestVerifiesAnElixirPackage(t *testing.T) {
	for _, want := range vectors.Load(t).Packages {
		pkg, err := capability.Verify(want.Signed)
		if err != nil {
			t.Fatalf("%s: %v", want.Path, err)
		}

		fields, _ := pkg["capability"].(map[string]any)
		if fields["id"] == nil {
			t.Errorf("%s: the package holds no capability", want.Path)
		}
	}
}

func TestRefusesAChangedPackage(t *testing.T) {
	want := vectors.Load(t).Packages[0]

	changes := map[string]func(map[string]any){
		"another title": func(signed map[string]any) {
			fields := signed["capability"].(map[string]any)
			fields["title"] = "Something else"
		},
		"another hash": func(signed map[string]any) {
			signed["package_hash"] = strings.Repeat("ab", 32)
		},
		"another signer": func(signed map[string]any) {
			signature := signed["signature"].(map[string]any)
			signature["signer_public_key"] = strings.Repeat("cd", 32)
		},
		"no signature": func(signed map[string]any) { delete(signed, "signature") },
	}

	for name, change := range changes {
		signed := copyDeep(want.Signed)
		change(signed)

		if _, err := capability.Verify(signed); err == nil {
			t.Errorf("%s: the package still verified", name)
		}
	}
}

func TestReadsTheManifestRequest(t *testing.T) {
	cases := []struct {
		method string
		path   string
		ask    capability.Ask
		id     string
	}{
		{"GET", "/info", capability.AskSummary, ""},
		{"get", "/info", capability.AskSummary, ""},
		{"GET", "/info/capabilities/primary", capability.AskDetail, "primary"},
		{"GET", "/info/capabilities/", capability.AskNone, ""},
		{"GET", "/info/capabilities/a/b", capability.AskNone, ""},
		{"POST", "/info", capability.AskNone, ""},
		{"GET", "/", capability.AskNone, ""},
	}

	for _, test := range cases {
		ask, id := capability.Request(map[string]any{"method": test.method, "path": test.path})
		if ask != test.ask || id != test.id {
			t.Errorf("%s %s gave %v %q", test.method, test.path, ask, id)
		}
	}
}

func TestServesTheSummaryAndTheDetail(t *testing.T) {
	me, _ := identity.Generate()

	pkg, err := capability.LoadFile(filepath.Join("..", "..", "providers", "exec", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}

	summary, err := capability.Summary(me, pkg)
	if err != nil {
		t.Fatal(err)
	}
	if summary["view"] != "summary" || summary["capability_count"] != 1 {
		t.Errorf("summary = %v", summary)
	}

	capabilities := capability.SummaryCapabilities(summary)
	if len(capabilities) != 1 || capabilities[0]["id"] != "primary" {
		t.Fatalf("the summary holds %v", capabilities)
	}
	if capabilities[0]["detail_path"] != "/info/capabilities/primary" {
		t.Errorf("detail path = %v", capabilities[0]["detail_path"])
	}

	detail, err := capability.Detail(me, pkg, "primary")
	if err != nil {
		t.Fatal(err)
	}
	if detail["view"] != "detail" {
		t.Errorf("detail = %v", detail["view"])
	}
	if _, err := capability.Verify(detail); err != nil {
		t.Errorf("the detail does not verify: %v", err)
	}

	if _, err := capability.Detail(me, pkg, "other"); err == nil {
		t.Error("a capability that is not there was served")
	}
}

func TestLoadsTOMLAndJSONTheSame(t *testing.T) {
	json := `{"release":{"version":"1.0.0","channel":"stable"},
	  "capability":{"id":"primary","kind":"compute","scheme":"exec","title":"T","summary":"S",
	  "invocation":{"method":"EXEC","path":"/"}}}`

	toml := "[release]\nversion = \"1.0.0\"\nchannel = \"stable\"\n\n" +
		"[capability]\nid = \"primary\"\nkind = \"compute\"\nscheme = \"exec\"\n" +
		"title = \"T\"\nsummary = \"S\"\n\n[capability.invocation]\nmethod = \"EXEC\"\npath = \"/\"\n"

	me, _ := identity.Generate()
	dir := t.TempDir()

	fromJSON := loadAndSign(t, me, filepath.Join(dir, "a.json"), json)
	fromTOML := loadAndSign(t, me, filepath.Join(dir, "a.toml"), toml)

	if fromJSON["package_hash"] != fromTOML["package_hash"] {
		t.Errorf("the two files gave different hashes: %v and %v", fromJSON["package_hash"], fromTOML["package_hash"])
	}
}

func TestRefusesAFileThatIsNotAPackage(t *testing.T) {
	dir := t.TempDir()

	for name, body := range map[string]string{
		"no capability": `{"release":{"version":"1.0.0"}}`,
		"no title":      `{"capability":{"id":"primary","kind":"compute","scheme":"exec"}}`,
		"not JSON":      `not json`,
	} {
		path := filepath.Join(dir, "bad.json")
		write(t, path, body)

		if _, err := capability.LoadFile(path); err == nil {
			t.Errorf("%s: the file passed", name)
		}
	}

	write(t, filepath.Join(dir, "bad.yaml"), "x: 1")
	if _, err := capability.LoadFile(filepath.Join(dir, "bad.yaml")); err != capability.ErrUnsupportedFile {
		t.Error("a file that is not JSON or TOML passed")
	}
}
