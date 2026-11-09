package architecture

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxLines is the most lines that a source file may hold. A file that
// grows past it holds more than one concern: split it by concern.
const maxLines = 600

// oversize is the debt: the source files that were over maxLines when this
// check came, each with the most lines it may hold now. An entry can only
// go down, and it goes when its file is at or under maxLines. Never add an
// entry.
var oversize = map[string]int{
	"adapters/relay/rings/rings.go":        1104,
	"adapters/transport/relay/relay.go":    1242,
	"apps/dm/cmd/arc-dm/conversations.go":  744,
	"apps/dm/cmd/arc-dm/main.go":           965,
	"apps/storage/cmd/arc-storage/main.go": 680,
	"apps/storage/server/server.go":        862,
	"cmd/arc/capability.go":                940,
	"cmd/arc/keys.go":                      1251,
	"core/mail/mail.go":                    1596,
	"runtime/citizen/selfsync.go":          816,
	"runtime/iface/data.go":                1437,
	"runtime/iface/entries.go":             963,
	"runtime/iface/manifest.go":            777,
	"runtime/iface/notebook.go":            637,
	"runtime/selfsync/network.go":          970,
	"runtime/selfsync/repair.go":           770,
	"sdk/storage/client.go":                623,
}

// TestFileSize refuses a source file over maxLines, and keeps oversize
// honest.
func TestFileSize(t *testing.T) {
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	for _, layer := range []string{"core", "sdk", "adapters", "runtime", "apps", "cmd", "internal"} {
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
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			relative, _ := filepath.Rel(root, path)
			relative = filepath.ToSlash(relative)
			if reason := tooLarge(relative, lineCount(data)); reason != "" {
				t.Error(reason)
			}
			seen[relative] = true
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for path := range oversize {
		if !seen[path] {
			t.Errorf("%s is in oversize but is not a source file: remove its entry", path)
		}
	}
}

// tooLarge gives the reason that a file of n lines breaks the rule, or "".
func tooLarge(path string, n int) string {
	ceiling, listed := oversize[path]
	switch {
	case listed && n <= maxLines:
		return fmt.Sprintf("%s is %d lines, at or under %d: remove its entry from oversize", path, n, maxLines)
	case listed && n > ceiling:
		return fmt.Sprintf("%s is %d lines, over its ceiling of %d: split it by concern", path, n, ceiling)
	case listed && n < ceiling:
		return fmt.Sprintf("%s is %d lines: lower its ceiling in oversize from %d to %d", path, n, ceiling, n)
	case !listed && n > maxLines:
		return fmt.Sprintf("%s is %d lines, over %d: split it by concern", path, n, maxLines)
	}
	return ""
}

// lineCount counts lines as wc -l does, plus a last line with no newline.
func lineCount(data []byte) int {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}
