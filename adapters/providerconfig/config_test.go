package providerconfig_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gezibash/arc/adapters/providerconfig"
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

	got, err := providerconfig.ConfigPath("TEST_CONFIG")
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
		if _, err := providerconfig.ConfigPath("TEST_CONFIG"); err == nil {
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
	if err := providerconfig.ReadConfig(write(t, "a.json", `{"grants":["x"],"cwd":"/tmp"}`), &good); err != nil {
		t.Fatal(err)
	}
	if len(good.Grants) != 1 || good.CWD != "/tmp" {
		t.Errorf("config = %+v", good)
	}

	var other config
	err := providerconfig.ReadConfig(write(t, "b.json", `{"grants":[],"unknown":1}`), &other)
	if err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("an unknown field gave %v", err)
	}

	if err := providerconfig.ReadConfig(write(t, "c.json", `not json`), &other); err == nil {
		t.Error("a file that is not JSON passed")
	}
	if err := providerconfig.ReadConfig("/does/not/exist", &other); err == nil {
		t.Error("a missing file passed")
	}
}

func TestGrants(t *testing.T) {
	key := strings.Repeat("ab", 32)

	grants, err := providerconfig.Grants([]string{key})
	if err != nil || !grants[key] {
		t.Fatalf("grants = %v, %v", grants, err)
	}

	for name, keys := range map[string][]string{
		"none":       {},
		"short":      {"abc"},
		"upper case": {strings.ToUpper(key)},
	} {
		if _, err := providerconfig.Grants(keys); err == nil {
			t.Errorf("%s: the grants passed", name)
		}
	}
}

func TestLimitAndDirectory(t *testing.T) {
	if err := providerconfig.Limit("timeout_ms", 500, 1000); err != nil {
		t.Error(err)
	}
	if err := providerconfig.Limit("timeout_ms", 0, 1000); err == nil {
		t.Error("zero passed")
	}
	if err := providerconfig.Limit("timeout_ms", 1001, 1000); err == nil {
		t.Error("a value over the ceiling passed")
	}

	if err := providerconfig.Directory("cwd", t.TempDir()); err != nil {
		t.Error(err)
	}
	if err := providerconfig.Directory("cwd", "relative"); err == nil {
		t.Error("a relative path passed")
	}
	if err := providerconfig.Directory("cwd", write(t, "file", "x")); err == nil {
		t.Error("a file passed as a directory")
	}
}
