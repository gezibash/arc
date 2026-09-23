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

	path, args, found, err := host.ParseServeURI("exec://" + binary + "?manifest=" + manifest + "&args=-v+--x")
	if err != nil || path != binary || found != manifest || len(args) != 2 || args[1] != "--x" {
		t.Fatalf("parse gave %q %v %q %v", path, args, found, err)
	}

	for _, uri := range []string{
		"http://" + binary,
		"exec://" + binary,
		"exec:///does/not/exist?manifest=" + manifest,
		"exec://" + filepath.Dir(binary) + "?manifest=" + manifest,
		"exec://" + binary + "?manifest=" + manifest + "&args=[\"-v\",",
	} {
		if _, _, _, err := host.ParseServeURI(uri); err == nil {
			t.Errorf("%s passed", uri)
		}
	}
}
