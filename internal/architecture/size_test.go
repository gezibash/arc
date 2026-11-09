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
	err := walkSources(filepath.Join("..", ".."), func(path, relative string) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if n := lineCount(data); n > maxLines {
			t.Errorf("%s is %d lines, over %d: split it by concern", relative, n, maxLines)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// walkSources calls visit for each source file under root that the size
// rules check: the Go files of each layer, without tests and testdata.
// relative is the path from root, with forward slashes.
func walkSources(root string, visit func(path, relative string) error) error {
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
			relative, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			return visit(path, filepath.ToSlash(relative))
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// lineCount counts lines as wc -l does, plus a last line with no newline.
func lineCount(data []byte) int {
	n := bytes.Count(data, []byte("\n"))
	if len(data) > 0 && data[len(data)-1] != '\n' {
		n++
	}
	return n
}
