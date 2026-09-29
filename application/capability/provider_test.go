package capability_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gezibash/arc/application/capability"
	"github.com/gezibash/arc/application/iface"
)

func TestModernProviderNeedsOnlyOneManifest(t *testing.T) {
	body, err := os.ReadFile("../../apps/exec/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	// The published definition must advertise the same default the host enforces.
	defaulted := bytes.Replace(body, []byte(`"max_bytes": 1048576`), []byte(`"max_bytes": 0`), 1)
	if err := os.WriteFile(path, defaulted, 0600); err != nil {
		t.Fatal(err)
	}
	definition, err := capability.LoadProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	announced, err := iface.Parse([]byte(definition.Content))
	if err != nil || announced.Service.MaxBytes != 1048576 {
		t.Fatalf("announced=%+v err=%v", announced, err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	p, err := capability.LoadProvider(path)
	if err != nil || p.ID != "exec" || p.MaxBytes != 1048576 {
		t.Fatalf("provider=%+v error=%v", p, err)
	}
	bad := bytes.Replace(body, []byte(`"max_bytes": 1048576`), []byte(`"max_bytes": -1`), 1)
	if bytes.Equal(bad, body) {
		t.Fatal("the fixture no longer contains the documented body limit")
	}
	if err := os.WriteFile(path, bad, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := capability.LoadProvider(path); err == nil {
		t.Fatal("accepted a negative request limit")
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}

	// A broken old manifest cannot override the current document beside it.
	if err := os.Rename(path, filepath.Join(dir, "interface.json")); err != nil {
		t.Fatal(err)
	}
	for _, old := range []string{"", "broken legacy JSON"} {
		if old != "" {
			if err := os.WriteFile(path, []byte(old), 0600); err != nil {
				t.Fatal(err)
			}
		}
		p, err := capability.LoadProvider(path)
		if err != nil || p.ID != "exec" || p.MaxBytes != 1048576 {
			t.Fatalf("legacy=%q provider=%+v error=%v", old, p, err)
		}
	}
}
