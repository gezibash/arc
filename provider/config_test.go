package provider_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/provider"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigPath(t *testing.T) {
	path := write(t, "config.json", "{}")
	t.Setenv("TEST_CONFIG", path)

	got, err := provider.ConfigPath("TEST_CONFIG")
	if err != nil || got != path {
		t.Fatalf("got %q, %v", got, err)
	}

	for name, value := range map[string]string{
		"empty":       "",
		"relative":    "config.json",
		"a directory": t.TempDir(),
		"not there":   "/does/not/exist/config.json",
	} {
		t.Setenv("TEST_CONFIG", value)
		if _, err := provider.ConfigPath("TEST_CONFIG"); err == nil {
			t.Errorf("%s: the path passed", name)
		}
	}
}

func TestReadConfig(t *testing.T) {
	type config struct {
		Grants []string `json:"grants"`
		CWD    string   `json:"cwd"`
	}

	var good config
	if err := provider.ReadConfig(write(t, "a.json", `{"grants":["x"],"cwd":"/tmp"}`), &good); err != nil {
		t.Fatal(err)
	}
	if len(good.Grants) != 1 || good.CWD != "/tmp" {
		t.Errorf("config = %+v", good)
	}

	var other config
	err := provider.ReadConfig(write(t, "b.json", `{"grants":[],"unknown":1}`), &other)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("an unknown field gave %v", err)
	}

	if err := provider.ReadConfig(write(t, "c.json", `not json`), &other); err == nil {
		t.Error("a file that is not JSON passed")
	}
	if err := provider.ReadConfig("/does/not/exist", &other); err == nil {
		t.Error("a missing file passed")
	}
}

func TestGrants(t *testing.T) {
	key := strings.Repeat("ab", 32)

	grants, err := provider.Grants([]string{key})
	if err != nil || !grants[key] {
		t.Fatalf("grants = %v, %v", grants, err)
	}

	for name, keys := range map[string][]string{
		"none":       {},
		"short":      {"abc"},
		"upper case": {strings.ToUpper(key)},
	} {
		if _, err := provider.Grants(keys); err == nil {
			t.Errorf("%s: the grants passed", name)
		}
	}
}

func TestLimitAndDirectory(t *testing.T) {
	if err := provider.Limit("timeout_ms", 500, 1000); err != nil {
		t.Error(err)
	}
	if err := provider.Limit("timeout_ms", 0, 1000); err == nil {
		t.Error("zero passed")
	}
	if err := provider.Limit("timeout_ms", 1001, 1000); err == nil {
		t.Error("a value over the ceiling passed")
	}

	if err := provider.Directory("cwd", t.TempDir()); err != nil {
		t.Error(err)
	}
	if err := provider.Directory("cwd", "relative"); err == nil {
		t.Error("a relative path passed")
	}
	if err := provider.Directory("cwd", write(t, "file", "x")); err == nil {
		t.Error("a file passed as a directory")
	}
}
