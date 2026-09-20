package capability_test

import (
	"os"
	"testing"

	"github.com/gezibash/arc/go/capability"
	"github.com/gezibash/arc/go/identity"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func loadAndSign(t *testing.T, me *identity.Identity, path, body string) map[string]any {
	t.Helper()
	write(t, path, body)

	pkg, err := capability.LoadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	signed, err := capability.Sign(me, pkg)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func copyDeep(value map[string]any) map[string]any {
	out := make(map[string]any, len(value))
	for key, item := range value {
		switch item := item.(type) {
		case map[string]any:
			out[key] = copyDeep(item)
		case []any:
			list := make([]any, len(item))
			for index, entry := range item {
				if fields, ok := entry.(map[string]any); ok {
					list[index] = copyDeep(fields)
				} else {
					list[index] = entry
				}
			}
			out[key] = list
		default:
			out[key] = item
		}
	}
	return out
}
