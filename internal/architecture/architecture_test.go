// Package architecture checks the dependency rules in AGENTS.md.
package architecture

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const module = "github.com/gezibash/arc/"

// TestPackageBoundaries checks production sources, including OS-specific files.
// It does not import the packages under inspection: an invalid dependency can be
// reported even when it would create a Go import cycle.
func TestPackageBoundaries(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, layer := range []string{"core", "sdk", "adapters", "runtime", "apps", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, layer), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			// The tests of an app are checked too: they must not need a
			// package that another repository cannot import.
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || (strings.HasSuffix(path, "_test.go") && layer != "apps") {
				return nil
			}
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				return err
			}
			for _, imp := range file.Imports {
				imported, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					return err
				}
				relative, _ := filepath.Rel(root, path)
				if reason := forbidden(filepath.ToSlash(relative), imported); reason != "" {
					t.Errorf("%s imports %s: %s", relative, imported, reason)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// forbidden gives the rule that an import breaks, or "" if it breaks none.
// file is the path of the source file from the root of the repository.
func forbidden(file, imported string) string {
	layer, rest, _ := strings.Cut(file, "/")
	if after, ok := strings.CutPrefix(imported, module); ok {
		target := after
		in := func(prefix string) bool { return target == prefix || strings.HasPrefix(target, prefix+"/") }
		switch layer {
		case "core":
			if !in("core") {
				return "core must depend only on core ports and rules"
			}
		case "sdk":
			if !in("core") && !in("sdk") {
				return "the SDK must depend only on core and the SDK"
			}
		case "adapters":
			if in("runtime") || in("apps") || in("cmd") {
				return "adapters must not depend on the runtime, on apps, or on entry points"
			}
		case "runtime":
			if in("apps") || in("cmd") {
				return "the runtime must not depend on concrete apps or on entry points"
			}
		case "apps":
			if strings.HasSuffix(file, "_test.go") {
				if in("internal") {
					return "the tests of an app must not depend on internal packages"
				}
				break
			}
			// An app must build in another repository. It uses the SDK and
			// its own packages, and nothing else of this module.
			app, _, _ := strings.Cut(rest, "/")
			if !in("sdk") && !in("apps/"+app) {
				return "an app must depend only on the SDK and its own packages"
			}
		case "cmd":
			if in("apps") {
				return "arc must not depend on a concrete app"
			}
		}
	}
	if layer == "core" {
		for _, concrete := range []string{
			"os", "syscall", "net", "net/http", "net/rpc", "database/sql",
			"github.com/blevesearch/bleve/v2", "go.etcd.io/bbolt", "zombiezen.com/go/sqlite", "modernc.org/sqlite",
			"fiatjaf.com/nostr/eventstore/boltdb", "fiatjaf.com/nostr/khatru",
			"fiatjaf.com/nostr/nip05", "fiatjaf.com/nostr/nip11",
			"fiatjaf.com/nostr/nip46", "fiatjaf.com/nostr/nip77",
		} {
			// net/url and net/netip are value/codec packages, not network I/O.
			if imported == "net/url" || imported == "net/netip" {
				continue
			}
			if imported == concrete || strings.HasPrefix(imported, concrete+"/") {
				return "concrete I/O belongs in an adapter"
			}
		}
	}
	return ""
}
