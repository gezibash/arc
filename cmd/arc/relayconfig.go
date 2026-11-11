package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

func relaysPath(dir string) string { return filepath.Join(dir, "relays") }

func indexersPath(dir string) string { return filepath.Join(dir, "indexers") }

// readRelays returns the relays that arc uses. A list that an older arc
// wrote can hold a line that is not a relay URL, such as the second half
// of a URL that held a newline (#441). arc skips such a line; arc relay ls
// shows it, and arc relay rm removes it.
func readRelays(dir string) ([]string, error) { return relayURLs(readURLs(relaysPath(dir))) }

func writeRelays(dir string, urls []string) error { return writeURLs(dir, relaysPath(dir), urls) }

// An indexer relay holds only relay lists. A citizen publishes its NIP-65
// and NIP-17 lists there, and looks up the lists of others there.
func readIndexers(dir string) ([]string, error) { return relayURLs(readURLs(indexersPath(dir))) }

// isRelayURL says whether a line of a relay list is a ws:// or wss:// URL.
func isRelayURL(line string) bool {
	return strings.HasPrefix(line, "ws://") || strings.HasPrefix(line, "wss://")
}

// relayURLs keeps the lines that are relay URLs.
func relayURLs(lines []string, err error) ([]string, error) {
	return slices.DeleteFunc(lines, func(line string) bool { return !isRelayURL(line) }), err
}

// checkRelayURL refuses a relay URL that is not ws:// or wss://, or that
// holds a space, a control character or a format character. The list keeps
// one relay on each line: a newline would add a second relay. A control
// character would reach the terminal in each line that names the relay,
// and a bidi or invisible character could make it look like another relay
// (#441).
func checkRelayURL(url string) error {
	if !isRelayURL(url) {
		return errors.New("a relay is a ws:// or wss:// URL")
	}
	if strings.ContainsFunc(url, func(r rune) bool { return unicode.IsControl(r) || unicode.IsSpace(r) || unicode.Is(unicode.Cf, r) }) {
		return errors.New("a relay URL holds no space, no control character and no format character")
	}
	return nil
}

func writeIndexers(dir string, urls []string) error {
	return writeURLs(dir, indexersPath(dir), urls)
}

func readURLs(path string) ([]string, error) {
	body, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

func writeURLs(dir, path string, urls []string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	body := strings.Join(urls, "\n")
	if body != "" {
		body += "\n"
	}
	return os.WriteFile(path, []byte(body), 0o600)
}
