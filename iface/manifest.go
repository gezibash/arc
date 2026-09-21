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
	"strings"
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
}

// Group is the NIP-29 group of the group kinds.
type Group struct {
	Relay string `json:"relay"`
	ID    string `json:"id"`
}

// Service holds the defaults of every call.
type Service struct {
	Method   string `json:"method"`
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes"`
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
	Output  Output   `json:"output,omitempty"`
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
	Call    *Call    `json:"call,omitempty"`
	Publish *Publish `json:"publish,omitempty"`
	Delete  *Delete  `json:"delete,omitempty"`
	Query   *Query   `json:"query,omitempty"`
	Watch   *Query   `json:"watch,omitempty"`
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
	Kind    string     `json:"kind"`
	D       string     `json:"d,omitempty"`
	Content Content    `json:"content,omitempty"`
	Tags    [][]string `json:"tags,omitempty"`
	Revise  string     `json:"revise,omitempty"`
	To      []string   `json:"to,omitempty"`
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

// fileFields are the fields that a file argument offers to templates.
var fileFields = []string{"name", "type", "size", "sha256"}

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

func (m *Manifest) check() error {
	if !idPattern.MatchString(m.ID) {
		return fmt.Errorf("the id %q must be lower-case letters, digits and hyphens", m.ID)
	}
	if !slices.Contains(shapes, m.Shape) {
		return fmt.Errorf("the shape %q must be service or data", m.Shape)
	}
	if m.Shape == "service" && m.Service == nil {
		return errors.New("a service needs a service section")
	}
	if m.Shape == "data" && m.Service != nil {
		return errors.New("a data capability has no service section")
	}

	grouped := false
	for name, kind := range m.Kinds {
		if !namePattern.MatchString(name) {
			return fmt.Errorf("the kind name %q must be lower-case letters, digits and underscores", name)
		}
		if kind.Kind < 0 || kind.Kind > 65535 {
			return fmt.Errorf("kind %s: %d is not an event kind", name, kind.Kind)
		}
		if !slices.Contains(visibilities, kind.Visibility) {
			return fmt.Errorf("kind %s: the visibility %q must be public, sealed, private or group", name, kind.Visibility)
		}
		grouped = grouped || kind.Visibility == "group"
	}
	if grouped && (m.Group == nil || m.Group.Relay == "" || m.Group.ID == "") {
		return errors.New("a group kind needs a group section with a relay and an id")
	}

	for name, format := range m.Formats {
		if format.Record != "" && format.Table != nil {
			return fmt.Errorf("format %s: a format has a record or a table, not both", name)
		}
		for _, template := range []string{format.Header, format.Record, format.Empty} {
			if _, err := compile(template, nil); err != nil {
				return fmt.Errorf("format %s: %w", name, err)
			}
		}
	}

	if len(m.Commands) == 0 {
		return errors.New("the manifest has no command")
	}
	paths := map[string]bool{}
	for _, c := range m.Commands {
		key := strings.Join(c.Path, " ")
		if paths[key] {
			return fmt.Errorf("two commands have the path %q", key)
		}
		paths[key] = true
		if err := m.checkCommand(c); err != nil {
			return fmt.Errorf("command %q: %w", key, err)
		}
	}
	return nil
}

func (m *Manifest) checkCommand(c Command) error {
	for _, word := range c.Path {
		if !wordPattern.MatchString(word) {
			return fmt.Errorf("the path word %q must be lower-case letters, digits and hyphens", word)
		}
	}

	names := slices.Clone(builtins)
	stdin := 0
	positionals := 0
	variadic := false
	for _, a := range c.Args {
		if !namePattern.MatchString(a.Name) || slices.Contains(names, a.Name) {
			return fmt.Errorf("the argument name %q is not valid, or not unique", a.Name)
		}
		names = append(names, a.Name)
		if !slices.Contains(argKinds, a.Kind) {
			return fmt.Errorf("argument %s: the kind %q must be positional, option or switch", a.Name, a.Kind)
		}
		if !slices.Contains(argTypes, a.Type) && a.Kind != "switch" {
			return fmt.Errorf("argument %s: the type %q does not exist", a.Name, a.Type)
		}
		if a.Name == "json" || a.Name == "later" || a.Name == "help" {
			return fmt.Errorf("argument %s: every command has --%s", a.Name, a.Name)
		}
		switch {
		case a.Type == "stdin":
			stdin++
		case a.Kind == "positional":
			if variadic {
				return fmt.Errorf("argument %s: only the last positional can be variadic", a.Name)
			}
			positionals++
			variadic = a.Variadic
		}
		if a.Variadic && (a.Kind != "positional" || a.Type != "text") {
			return fmt.Errorf("argument %s: only a positional text can be variadic", a.Name)
		}
		if a.Type == "address" {
			if a.Pattern == "" {
				return fmt.Errorf("argument %s: an address needs a pattern", a.Name)
			}
			if _, err := regexp.Compile(a.Pattern); err != nil {
				return fmt.Errorf("argument %s: the pattern does not compile: %w", a.Name, err)
			}
		}
	}
	if stdin > 1 {
		return errors.New("a command has at most one stdin argument")
	}

	check := func(template string) error {
		_, err := compile(template, func(root string) bool {
			return slices.Contains(names, root)
		})
		return err
	}

	actions := 0
	a := c.Action
	if a.Call != nil {
		actions++
		if m.Shape != "service" {
			return errors.New("only a service has call")
		}
		if !slices.Contains(classes, a.Call.Class) {
			return fmt.Errorf("the call class %q must be live or later", a.Call.Class)
		}
		for _, t := range []string{a.Call.Method, a.Call.Path, a.Call.Body} {
			if err := check(t); err != nil {
				return err
			}
		}
	}
	if a.Publish != nil {
		actions++
		if err := m.checkKind(a.Publish.Kind); err != nil {
			return err
		}
		switch visibility := m.Kinds[a.Publish.Kind].Visibility; {
		case visibility == "sealed" && a.Publish.D == "":
			return errors.New("a publish of a sealed kind needs a d tag")
		case visibility == "private" && len(a.Publish.To) == 0:
			return errors.New("a publish of a private kind needs recipients in to")
		case visibility != "private" && len(a.Publish.To) > 0:
			return errors.New("only a private kind has recipients")
		}
		if a.Publish.Revise != "" && a.Publish.Revise != "replace" && a.Publish.Revise != "append" {
			return fmt.Errorf("revise %q must be replace or append", a.Publish.Revise)
		}
		templates := append([]string{a.Publish.D, a.Publish.Content.Text}, a.Publish.To...)
		for _, tag := range a.Publish.Tags {
			templates = append(templates, tag...)
		}
		templates = append(templates, jsonTemplates(a.Publish.Content.JSON)...)
		for _, t := range templates {
			if err := check(t); err != nil {
				return err
			}
		}
	}
	if a.Delete != nil {
		actions++
		if err := m.checkKind(a.Delete.Kind); err != nil {
			return err
		}
		if err := check(a.Delete.D); err != nil {
			return err
		}
	}
	for _, q := range []*Query{a.Query, a.Watch} {
		if q == nil {
			continue
		}
		actions++
		if len(q.Kinds) == 0 {
			return errors.New("a query names at least one kind")
		}
		for _, k := range q.Kinds {
			if err := m.checkKind(k); err != nil {
				return err
			}
			if m.Kinds[k].Visibility != m.Kinds[q.Kinds[0]].Visibility {
				return errors.New("a query reads kinds of one visibility")
			}
		}
		templates := []string{q.D, q.IDs, string(q.Since), string(q.Until), string(q.Limit)}
		switch q.Authors {
		case "", "me", "author", "any":
		default:
			templates = append(templates, q.Authors)
		}
		for _, v := range q.Tags {
			templates = append(templates, v)
		}
		for _, t := range templates {
			if err := check(t); err != nil {
				return err
			}
		}
	}
	if actions != 1 {
		return fmt.Errorf("a command has exactly one action, not %d", actions)
	}

	o := c.Output
	if o.Open != nil && !slices.Contains(parses, o.Open.Parse) {
		return fmt.Errorf("open parses text, json or frontmatter, not %q", o.Open.Parse)
	}
	if o.Format != "" {
		if _, ok := m.Formats[o.Format]; !ok {
			return fmt.Errorf("the format %q does not exist", o.Format)
		}
	}
	if o.Sort != nil && o.Sort.Order != "" && o.Sort.Order != "asc" && o.Sort.Order != "desc" {
		return fmt.Errorf("sort order %q must be asc or desc", o.Sort.Order)
	}
	if o.Save != nil {
		if o.Save.Decode != "" && o.Save.Decode != "base64" {
			return fmt.Errorf("save decodes base64, not %q", o.Save.Decode)
		}
		for _, t := range []string{o.Save.To, o.Save.SHA256} {
			if _, err := compile(t, nil); err != nil {
				return err
			}
		}
	}
	if o.Tail != nil && a.Watch == nil {
		return errors.New("tail works only in a watch")
	}
	var conds func([]Cond) error
	conds = func(list []Cond) error {
		for _, cond := range list {
			for _, t := range []string{cond.Is, cond.Not, cond.Prefix, cond.Lacks} {
				if err := check(t); err != nil {
					return err
				}
			}
			if err := conds(cond.Any); err != nil {
				return err
			}
		}
		return nil
	}
	if err := conds(o.Where); err != nil {
		return err
	}
	for _, t := range []string{joinLines(o.Join), rankQuery(o.Rank)} {
		if err := check(t); err != nil {
			return err
		}
	}
	return nil
}

func joinLines(j *Join) string {
	if j == nil {
		return ""
	}
	return j.Lines
}

func rankQuery(r *Rank) string {
	if r == nil {
		return ""
	}
	return r.Query
}

func (m *Manifest) checkKind(name string) error {
	if _, ok := m.Kinds[name]; !ok {
		return fmt.Errorf("the kind name %q is not in kinds", name)
	}
	return nil
}

// jsonTemplates lists every string inside a JSON value.
func jsonTemplates(v any) []string {
	var out []string
	switch v := v.(type) {
	case string:
		out = append(out, v)
	case map[string]any:
		for _, item := range v {
			out = append(out, jsonTemplates(item)...)
		}
	case []any:
		for _, item := range v {
			out = append(out, jsonTemplates(item)...)
		}
	}
	return out
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
