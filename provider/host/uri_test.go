package host_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gezibash/arc/provider/host"
)

func TestParseServeURI(t *testing.T) {
	// The test binary stands for the runtime: it is a program that exists.
	binary, _ := filepath.Abs(os.Args[0])
	manifest, _ := filepath.Abs(filepath.Join("testdata", "echo", "manifest.json"))

	path, args, found, cwd, err := host.ParseServeURI("exec://" + binary + "?manifest=" + manifest + "&args=-v+--x")
	if err != nil || path != binary || found != manifest || len(args) != 2 || args[1] != "--x" || cwd != "" {
		t.Fatalf("parse gave %q %v %q %q %v", path, args, found, cwd, err)
	}

	// A hand-written address can name the directory of the program.
	dir := t.TempDir()
	if _, _, _, cwd, err := host.ParseServeURI("exec://" + binary + "?manifest=" + manifest + "&cwd=" + dir); err != nil || cwd != dir {
		t.Fatalf("cwd = %q, %v, want %s", cwd, err, dir)
	}

	for _, uri := range []string{
		"http://" + binary,
		"exec://" + binary,
		"exec:///does/not/exist?manifest=" + manifest,
		"exec://" + filepath.Dir(binary) + "?manifest=" + manifest,
		"exec://" + binary + "?manifest=" + manifest + "&args=[oops",
		"exec://" + binary + "?manifest=" + manifest + "&cwd=/does/not/exist",
	} {
		if _, _, _, _, err := host.ParseServeURI(uri); err == nil {
			t.Errorf("%s passed", uri)
		}
	}
}
