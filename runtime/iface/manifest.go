// Package iface reads the manifests of the capability interface, version 1,
// and runs their commands. See docs/interface/SPEC.md.
//
// A manifest names primitives, and this package runs them. A manifest holds
// no code.
package iface

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"fiatjaf.com/nostr"
	"github.com/gezibash/arc/core/session"
)

// Version is the version of the interface that this package reads.
const Version = 1

// Manifest is one capability, as its author announced it.
type Manifest struct {
	Interface int               `json:"interface"`
	ID        string            `json:"id"`
	Title     string            `json:"title"`
	Summary   string            `json:"summary"`
	Shape     string            `json:"shape"`
	Kinds     map[string]Kind   `json:"kinds"`
	Group     *Group            `json:"group,omitempty"`
	Service   *Service          `json:"service,omitempty"`
	Formats   map[string]Format `json:"formats"`
	Commands  []Command         `json:"commands"`
}

// Kind is one event kind that the commands publish or read.
type Kind struct {
	Kind       int    `json:"kind"`
	Visibility string `json:"visibility"`
	// Frontmatter names nonempty fields required when this kind is published.
	Frontmatter   []string `json:"frontmatter,omitempty"`
	NotebookIndex string   `json:"notebook_index,omitempty"`
	// NotebookEntry names the kind of the entries of a notebook page. A
	// page then keeps each entry, and its head is never revised.
	NotebookEntry string `json:"notebook_entry,omitempty"`
}

// Group is the NIP-29 group of the group kinds.
type Group struct {
	Relay string `json:"relay"`
	ID    string `json:"id"`
}

// Service holds the defaults of every call.
type Service struct {
	Interactions []session.Mode `json:"interactions,omitempty"`
	Method       string         `json:"method"`
	Path         string         `json:"path"`
	MaxBytes     int            `json:"max_bytes"`
	// Output shows the reply of a call by address, arc call. Without it,
	// arc call writes the reply as it came.
	Output *Output `json:"output,omitempty"`
}

// Format renders records as text.
type Format struct {
	Header string `json:"header,omitempty"`
	Record string `json:"record,omitempty"`
	Empty  string `json:"empty,omitempty"`
	Table  *Table `json:"table,omitempty"`
}

// Table names the record fields that hold the columns and the rows.
type Table struct {
	Columns string `json:"columns"`
	Rows    string `json:"rows"`
}

// Command is one entry that the manifest adds to arc.
type Command struct {
	Path    []string `json:"path"`
	Summary string   `json:"summary"`
	Args    []Arg    `json:"args,omitempty"`
	Action  Action   `json:"action"`
	Output  Output   `json:"output"`
}

// Arg is one argument of a command.
type Arg struct {
	Name     string `json:"name"`
	Kind     string `json:"kind"`
	Type     string `json:"type"`
	Variadic bool   `json:"variadic,omitempty"`
	Required bool   `json:"required,omitempty"`
	Default  string `json:"default,omitempty"`
	Pattern  string `json:"pattern,omitempty"`
}

// Action is what a command does. Exactly one field is set.
type Action struct {
	Call     *Call     `json:"call,omitempty"`
	Publish  *Publish  `json:"publish,omitempty"`
	Delete   *Delete   `json:"delete,omitempty"`
	Query    *Query    `json:"query,omitempty"`
	Watch    *Query    `json:"watch,omitempty"`
	Notebook *Notebook `json:"notebook,omitempty"`
}

// Call sends one request to the provider.
type Call struct {
	Class  string `json:"class"`
	Method string `json:"method,omitempty"`
	Path   string `json:"path,omitempty"`
	Body   string `json:"body,omitempty"`
}

// Publish makes one event.
type Publish struct {
	Kind               string     `json:"kind"`
	D                  string     `json:"d,omitempty"`
	Content            Content    `json:"content"`
	Tags               [][]string `json:"tags,omitempty"`
	Revise             string     `json:"revise,omitempty"`
	To                 []string   `json:"to,omitempty"`
	FrontmatterAddress string     `json:"frontmatter_address,omitempty"`
	// Body names a file argument, as "{{file}}". Its raw bytes go beside
	// the draft as its body, not into the content. Only a sealed kind has
	// a body.
	Body string `json:"body,omitempty"`
	// at is the time of an entry, which a notebook sets. A manifest cannot.
	at nostr.Timestamp
}

// bodyPattern is the one form of a body: the name of a file argument.
var bodyPattern = regexp.MustCompile(`^\{\{\s*([a-z][a-z0-9_]*)\s*\}\}$`)

// bodyArg is the name of the file argument that a publish takes as its
// body, or "".
func bodyArg(p *Publish) string {
	if p == nil {
		return ""
	}
	if m := bodyPattern.FindStringSubmatch(p.Body); m != nil {
		return m[1]
	}
	return ""
}

// Content is a template for text, or an object whose values are templates.
type Content struct {
	Text string
	JSON map[string]any
}

// UnmarshalJSON reads a string, or {"json": {...}}.
func (c *Content) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &c.Text); err == nil {
		return nil
	}
	var object struct {
		JSON map[string]any `json:"json"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&object); err != nil || object.JSON == nil {
		return errors.New(`content must be a template, or {"json": {...}}`)
	}
	c.JSON = object.JSON
	return nil
}

// MarshalJSON writes the content back as it was read.
func (c Content) MarshalJSON() ([]byte, error) {
	if c.JSON != nil {
		return json.Marshal(map[string]any{"json": c.JSON})
	}
	return json.Marshal(c.Text)
}

// Delete asks relays to remove events.
type Delete struct {
	Kind string `json:"kind"`
	D    string `json:"d,omitempty"`
}

// Query reads events. A watch is a query that stays open.
type Query struct {
	Kinds   []string          `json:"kinds"`
	Authors string            `json:"authors,omitempty"`
	D       string            `json:"d,omitempty"`
	Tags    map[string]string `json:"tags,omitempty"`
	IDs     string            `json:"ids,omitempty"`
	History bool              `json:"history,omitempty"`
	Since   Number            `json:"since,omitempty"`
	Until   Number            `json:"until,omitempty"`
	Limit   Number            `json:"limit,omitempty"`
}

// Number is a whole number, or a template that gives one.
type Number string

// UnmarshalJSON reads a number or a string.
func (n *Number) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*n = Number(text)
		return nil
	}
	var number int64
	if err := json.Unmarshal(data, &number); err != nil {
		return errors.New("must be a whole number or a template")
	}
	*n = Number(strconv.FormatInt(number, 10))
	return nil
}

// Output is the pipeline of a command.
type Output struct {
	Open   *Open     `json:"open,omitempty"`
	Join   *Join     `json:"join,omitempty"`
	Where  []Cond    `json:"where,omitempty"`
	Latest *Latest   `json:"latest,omitempty"`
	Rank   *Rank     `json:"rank,omitempty"`
	Thread *Thread   `json:"thread,omitempty"`
	Sort   *Sort     `json:"sort,omitempty"`
	Limit  int       `json:"limit,omitempty"`
	Tail   *struct{} `json:"tail,omitempty"`
	Save   *Save     `json:"save,omitempty"`
	Format string    `json:"format,omitempty"`
	Exit   []Exit    `json:"exit,omitempty"`
}

// Exit sets the exit status of the command from its first record. Core
// takes the first rule whose conditions the record meets.
type Exit struct {
	Where []Cond `json:"where,omitempty"`
	// Code is a template over the record. A whole number from 0 to 255 is
	// the exit status. Any other value gives the status 1.
	Code string `json:"code"`
}

// Open parses the content of each record.
type Open struct {
	Parse string `json:"parse"`
}

// Join puts the whole content of each record in text.
type Join struct {
	Lines string `json:"lines,omitempty"`
}

// Cond is one condition of where.
type Cond struct {
	Field  string `json:"field,omitempty"`
	Is     string `json:"is,omitempty"`
	Not    string `json:"not,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	Lacks  string `json:"lacks,omitempty"`
	Any    []Cond `json:"any,omitempty"`
}

// Latest keeps the newest record for each value of a field.
type Latest struct {
	By string `json:"by"`
}

// Rank orders records by BM25.
type Rank struct {
	Query  string   `json:"query"`
	Fields []string `json:"fields"`
	Limit  int      `json:"limit,omitempty"`
}

// Thread orders records as replies.
type Thread struct {
	Parent string `json:"parent"`
}

// Sort orders records by a field.
type Sort struct {
	Field string `json:"field"`
	Order string `json:"order,omitempty"`
}

// Save writes a field to a path argument.
type Save struct {
	Field  string `json:"field"`
	To     string `json:"to"`
	SHA256 string `json:"sha256,omitempty"`
	Decode string `json:"decode,omitempty"`
}

// The words that a manifest can use.
var (
	shapes       = []string{"service", "data"}
	visibilities = []string{"public", "sealed", "private", "group"}
	argKinds     = []string{"positional", "option", "switch"}
	argTypes     = []string{"text", "integer", "key", "event", "file", "path", "address", "lines", "stdin"}
	classes      = []string{"live", "later"}
	parses       = []string{"text", "json", "frontmatter"}
	idPattern    = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	wordPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
)

// builtins are the names that every action template knows.
var builtins = []string{"me", "author", "now"}

// Parse reads and checks one manifest. It refuses a field that version 1
// does not define, so a typing error does not pass unseen.
func Parse(data []byte) (*Manifest, error) {
	var probe struct {
		Interface int `json:"interface"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("iface: the manifest is not JSON: %w", err)
	}
	switch {
	case probe.Interface == 0:
		return nil, errors.New("iface: the manifest names no interface version")
	case probe.Interface > Version:
		return nil, fmt.Errorf("iface: the manifest needs interface version %d; this arc reads version %d", probe.Interface, Version)
	case probe.Interface != Version:
		return nil, fmt.Errorf("iface: interface version %d does not exist", probe.Interface)
	}

	var m Manifest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return nil, fmt.Errorf("iface: %w", err)
	}
	if err := m.check(); err != nil {
		return nil, fmt.Errorf("iface: %s: %w", m.ID, err)
	}
	return &m, nil
}

// Find returns the command with the longest path that starts the words, and
// the words that follow it.
func (m *Manifest) Find(words []string) (*Command, []string, bool) {
	var best *Command
	for i := range m.Commands {
		c := &m.Commands[i]
		if len(c.Path) > len(words) || !slices.Equal(c.Path, words[:len(c.Path)]) {
			continue
		}
		if best == nil || len(c.Path) > len(best.Path) {
			best = c
		}
	}
	if best == nil {
		return nil, words, false
	}
	return best, words[len(best.Path):], true
}
