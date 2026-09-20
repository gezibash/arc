package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

var publicKeyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ConfigPath reads one environment variable that must name an absolute path
// to a regular file. Every provider reads its configuration this way, and
// fails closed when the operator did not set it.
func ConfigPath(name string) (string, error) {
	path := os.Getenv(name)
	if path == "" || !filepath.IsAbs(path) {
		return "", fmt.Errorf("%s must be an absolute path", name)
	}

	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must name a regular file", name)
	}
	return path, nil
}

// ReadConfig reads a JSON file into a value. A field that the value does not
// hold is an error, so a typing mistake in the configuration never passes
// without notice.
func ReadConfig(path string, into any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s is not readable", path)
	}

	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return fmt.Errorf("%s: %v", filepath.Base(path), err)
	}
	if decoder.More() {
		return fmt.Errorf("%s holds more than one document", filepath.Base(path))
	}
	return nil
}

// Grants turns a list of public keys into the set that a provider checks. The
// list must hold at least one key, and each key is 64 characters of lower
// case hex.
func Grants(keys []string) (map[string]bool, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("grants must be a list of public keys, and must hold one key")
	}

	grants := make(map[string]bool, len(keys))
	for _, key := range keys {
		if !publicKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("grants must hold 64 characters of lower case hex")
		}
		grants[key] = true
	}
	return grants, nil
}

// Limit checks one limit of the configuration against its ceiling.
func Limit(name string, value, max int) error {
	if value < 1 || value > max {
		return fmt.Errorf("limits.%s must be a whole number from 1 to %d", name, max)
	}
	return nil
}

// Directory checks that a path is absolute and names a directory.
func Directory(name, path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%s must be an absolute path", name)
	}

	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s must name a directory", name)
	}
	return nil
}
