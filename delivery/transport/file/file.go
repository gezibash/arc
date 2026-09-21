// Package file moves events through a directory: a USB stick, a shared
// folder, or a disk that a person carries from one machine to another.
//
// Each event is one file, events/<id>.json. A file is written once, under a
// temporary name, and then renamed, so a reader never sees half an event.
package file

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/delivery/transport"
)

// Dir is one directory.
type Dir struct {
	Path string
}

func (d Dir) events() string { return filepath.Join(d.Path, "events") }

// Name says which directory this is.
func (d Dir) Name() string { return "file:" + d.Path }

// Send writes one event, unless the directory already holds it.
func (d Dir) Send(_ context.Context, event nostr.Event) error {
	if err := os.MkdirAll(d.events(), 0o700); err != nil {
		return err
	}

	path := filepath.Join(d.events(), event.ID.Hex()+".json")
	if _, err := os.Stat(path); err == nil {
		return nil
	}

	body, err := json.Marshal(event)
	if err != nil {
		return err
	}

	temporary := path + ".new"
	if err := os.WriteFile(temporary, body, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// Fetch reads every event in the directory that matches the filter. A file
// that does not parse counts as unreadable. The store verifies the rest.
func (d Dir) Fetch(_ context.Context, filter nostr.Filter) (transport.Batch, error) {
	entries, err := os.ReadDir(d.events())
	if errors.Is(err, os.ErrNotExist) {
		return transport.Batch{}, nil
	}
	if err != nil {
		return transport.Batch{}, err
	}

	var batch transport.Batch
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		body, err := os.ReadFile(filepath.Join(d.events(), entry.Name()))
		if err != nil {
			batch.Unreadable++
			continue
		}

		var event nostr.Event
		if err := json.Unmarshal(body, &event); err != nil {
			batch.Unreadable++
			continue
		}
		if filter.Matches(event) {
			batch.Events = append(batch.Events, event)
		}
	}
	return batch, nil
}
