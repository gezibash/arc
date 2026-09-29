package host

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseServeURI reads exec:///path/to/runtime?manifest=/path/to/file.json,
// with an optional args parameter and an optional cwd parameter. No other
// form exists.
//
// The args parameter is a JSON list of strings, for example
// args=["-u","server.py"]. An argument in the list can contain a space.
// `arc serve` writes this form for a bundle. If the value does not start
// with "[", ParseServeURI splits it at each space, so a hand-written address
// can say args=-v+--x. If the value starts with "[" and is not a JSON list
// of strings, ParseServeURI returns an error.
//
// The cwd parameter names the directory that the program runs in. Without
// it, the program runs in the directory of arc.
func ParseServeURI(raw string) (path string, args []string, manifest string, cwd string, err error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "exec" {
		return "", nil, "", "", fmt.Errorf("serve: the address must be exec:///path/to/runtime?manifest=/path/to/file")
	}

	path = parsed.Path
	if path == "" {
		return "", nil, "", "", fmt.Errorf("serve: the address names no runtime")
	}
	if path, err = filepath.Abs(path); err != nil {
		return "", nil, "", "", err
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "", nil, "", "", fmt.Errorf("serve: %s is not a program", path)
	}

	query := parsed.Query()
	manifest = query.Get("manifest")
	if manifest == "" {
		return "", nil, "", "", fmt.Errorf("serve: the address names no manifest")
	}
	if manifest, err = filepath.Abs(manifest); err != nil {
		return "", nil, "", "", err
	}

	given := strings.TrimSpace(query.Get("args"))
	switch {
	case strings.HasPrefix(given, "["):
		if err := json.Unmarshal([]byte(given), &args); err != nil {
			return "", nil, "", "", fmt.Errorf("serve: the args of the address are not a JSON list of strings: %w", err)
		}
	case given != "":
		args = strings.Fields(given)
	}

	if cwd = query.Get("cwd"); cwd != "" {
		if cwd, err = filepath.Abs(cwd); err != nil {
			return "", nil, "", "", err
		}
		if info, err := os.Stat(cwd); err != nil || !info.IsDir() {
			return "", nil, "", "", fmt.Errorf("serve: the cwd %s is not a directory", cwd)
		}
	}
	return path, args, manifest, cwd, nil
}
