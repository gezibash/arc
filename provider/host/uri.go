package host

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseServeURI reads exec:///path/to/runtime?manifest=/path/to/file.json,
// with an optional args parameter. No other form exists.
func ParseServeURI(raw string) (path string, args []string, manifest string, err error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "exec" {
		return "", nil, "", fmt.Errorf("serve: the address must be exec:///path/to/runtime?manifest=/path/to/file")
	}

	path = parsed.Path
	if path == "" {
		return "", nil, "", fmt.Errorf("serve: the address names no runtime")
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", nil, "", err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", nil, "", fmt.Errorf("serve: %s is not a program", path)
	}

	query := parsed.Query()
	manifest = query.Get("manifest")
	if manifest == "" {
		return "", nil, "", fmt.Errorf("serve: the address names no manifest")
	}
	if manifest, err = filepath.Abs(manifest); err != nil {
		return "", nil, "", err
	}

	if given := query.Get("args"); given != "" {
		args = strings.Fields(given)
	}
	return path, args, manifest, nil
}
