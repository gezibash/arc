package toolbox

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/sealedbox"
)

// The cache keeps the answers of a command on this machine. Every record is
// sealed to the key of the owner, so the file tells a reader nothing.
//
// Layout: <Dir>/cache/<command>/<owner hex>/<record id>.sealed
// The file <Dir>/cache/<command>/enabled marks the cache of that command as
// on. Without it, the command keeps nothing. Each command is its own cache.

// Cache holds the records of one machine.
type Cache struct {
	// Dir is the directory of ARC, for example ~/.config/arc.
	Dir string
}

func (c *Cache) root() string { return filepath.Join(c.Dir, "cache") }

func (c *Cache) commandDir(command string) string {
	return filepath.Join(c.root(), NormalizeCommand(command))
}

func (c *Cache) flagPath(command string) string {
	return filepath.Join(c.commandDir(command), "enabled")
}

func (c *Cache) dir(command string, owner []byte) string {
	return filepath.Join(c.commandDir(command), hex.EncodeToString(owner))
}

// On says whether one command keeps records.
func (c *Cache) On(command string) bool {
	_, err := os.Stat(c.flagPath(command))
	return err == nil
}

// Enable turns the cache of one command on.
func (c *Cache) Enable(command string) error {
	if err := os.MkdirAll(c.commandDir(command), 0o700); err != nil {
		return err
	}
	return os.WriteFile(c.flagPath(command), []byte("on\n"), 0o600)
}

// Disable turns the cache of one command off. The records stay until the
// owner clears them.
func (c *Cache) Disable(command string) error {
	err := os.Remove(c.flagPath(command))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Clear removes the records of one command, or of every command when the
// name is empty. It returns the number of records that it removed.
func (c *Cache) Clear(command string, owner []byte) (int, error) {
	count, err := c.Count(command, owner)
	if err != nil {
		return 0, err
	}

	names := []string{command}
	if command == "" {
		names = c.commands()
	}

	for _, name := range names {
		if err := os.RemoveAll(c.dir(name, owner)); err != nil {
			return 0, err
		}
	}
	return count, nil
}

// Count returns the number of records of one command, or of every command
// when the name is empty.
func (c *Cache) Count(command string, owner []byte) (int, error) {
	names := []string{command}
	if command == "" {
		names = c.commands()
	}

	total := 0
	for _, name := range names {
		files, err := c.files(name, owner)
		if err != nil {
			return 0, err
		}
		total += len(files)
	}
	return total, nil
}

// Commands returns the commands that hold records of this owner.
func (c *Cache) Commands(owner []byte) []string {
	var out []string
	for _, name := range c.commands() {
		if files, err := c.files(name, owner); err == nil && len(files) > 0 {
			out = append(out, name)
		}
	}
	return out
}

// Keep seals every record of an answer and writes it. A record that is
// already there stays as it is, because its identifier names the message.
// Keep answers the text that it received, so it can stand as a filter.
func (c *Cache) Keep(command string, me *identity.Identity, text string) (int, error) {
	if !c.On(command) {
		return 0, nil
	}

	dir := c.dir(command, me.PublicKey)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 0, err
	}

	kept := 0
	for _, record := range Records(text) {
		if record.ID == "" || strings.ContainsAny(record.ID, "/\\.") {
			continue
		}

		path := filepath.Join(dir, record.ID+".sealed")
		if _, err := os.Stat(path); err == nil {
			continue
		}

		body, err := json.Marshal(map[string]any{
			"id": record.ID, "direction": record.Direction, "peer": record.Peer,
			"t": record.T, "reply_to": record.ReplyTo, "state": record.State,
			"body": record.Body,
		})
		if err != nil {
			return kept, err
		}

		sealed, err := sealedbox.SealTo(me.PublicKey, body)
		if err != nil {
			return kept, err
		}
		if err := os.WriteFile(path, sealed, 0o600); err != nil {
			return kept, err
		}
		kept++
	}
	return kept, nil
}

// Search opens the records of a command and answers the ones that hold the
// text. The search reads the body and the name of the peer, and ignores
// case. An empty command reads every command.
func (c *Cache) Search(command string, me *identity.Identity, query string) ([]Record, error) {
	names := []string{command}
	if command == "" {
		names = c.Commands(me.PublicKey)
	}
	query = strings.ToLower(query)

	var out []Record
	for _, name := range names {
		files, err := c.files(name, me.PublicKey)
		if err != nil {
			return nil, err
		}

		for _, path := range files {
			record, err := c.open(path, me)
			if err != nil {
				// A record that this key cannot open belongs to another
				// identity of the same machine. Pass over it.
				continue
			}
			if query == "" ||
				strings.Contains(strings.ToLower(record.Body), query) ||
				strings.Contains(strings.ToLower(record.Peer), query) {
				out = append(out, record)
			}
		}
	}

	sort.Slice(out, func(a, b int) bool { return out[a].T < out[b].T })
	return out, nil
}

func (c *Cache) open(path string, me *identity.Identity) (Record, error) {
	sealed, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}

	plain, err := sealedbox.Open(me, sealed)
	if err != nil {
		return Record{}, err
	}

	var held struct {
		ID        string `json:"id"`
		Direction string `json:"direction"`
		Peer      string `json:"peer"`
		T         string `json:"t"`
		ReplyTo   string `json:"reply_to"`
		State     string `json:"state"`
		Body      string `json:"body"`
	}
	if err := json.Unmarshal(plain, &held); err != nil {
		return Record{}, err
	}

	return Record{
		ID: held.ID, Direction: held.Direction, Peer: held.Peer, T: held.T,
		ReplyTo: held.ReplyTo, State: held.State, Body: held.Body,
	}, nil
}

// commands returns every command directory of the cache.
func (c *Cache) commands() []string {
	entries, err := os.ReadDir(c.root())
	if err != nil {
		return nil
	}

	var out []string
	for _, entry := range entries {
		if entry.IsDir() {
			out = append(out, entry.Name())
		}
	}
	return out
}

// files returns the sealed records of one command.
func (c *Cache) files(command string, owner []byte) ([]string, error) {
	entries, err := os.ReadDir(c.dir(command, owner))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var out []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sealed") {
			out = append(out, filepath.Join(c.dir(command, owner), entry.Name()))
		}
	}
	return out, nil
}

// Line writes one record as the thread writes it.
func (r Record) Line() string {
	return fmt.Sprintf("%s\t%s\t%s\t%s\t%s", r.T, r.Direction, r.Peer, shortID(r.ID),
		strings.ReplaceAll(r.Body, "\n", " "))
}
