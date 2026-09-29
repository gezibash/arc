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
	for _, layer := range []string{"core", "adapters", "application"} {
		err := filepath.WalkDir(filepath.Join(root, layer), func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
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
				if reason := forbidden(layer, imported); reason != "" {
					relative, _ := filepath.Rel(root, path)
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

func forbidden(layer, imported string) string {
	if strings.HasPrefix(imported, module) {
		target := strings.TrimPrefix(imported, module)
		if layer == "core" && target != "core" && !strings.HasPrefix(target, "core/") {
			return "core must depend only on core ports and rules"
		}
		if (layer == "adapters" || layer == "application") && (strings.HasPrefix(target, "cmd/") || strings.HasPrefix(target, "examples/")) {
			return "library packages must not depend on executable applications"
		}
		if layer == "adapters" && strings.HasPrefix(target, "application/") {
			return "adapters must not depend on application workflows"
		}
	}
	if layer == "core" {
		for _, concrete := range []string{
			"os", "syscall", "net", "net/http", "net/rpc", "database/sql",
			"go.etcd.io/bbolt", "zombiezen.com/go/sqlite", "modernc.org/sqlite",
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
