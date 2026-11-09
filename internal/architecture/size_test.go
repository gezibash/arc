package architecture

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxLines is the most lines that a source file may hold. A file that
// grows past it holds more than one concern: split it by concern.
const maxLines = 600

// TestFileSize refuses a source file over maxLines.
func TestFileSize(t *testing.T) {
	root := filepath.Join("..", "..")
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
			if n := lineCount(data); n > maxLines {
				relative, _ := filepath.Rel(root, path)
				t.Errorf("%s is %d lines, over %d: split it by concern", filepath.ToSlash(relative), n, maxLines)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// lineCount counts lines as wc -l does, plus a last line with no newline.
func lineCount(data []byte) int {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}
