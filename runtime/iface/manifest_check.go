package iface

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/gezibash/arc/core/session"
	"github.com/gezibash/arc/runtime/iface/template"
)

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
	if m.Service != nil && m.Service.MaxBytes < 0 {
		return errors.New("service.max_bytes must not be negative")
	}
	if m.Service != nil {
		seen := map[session.Mode]bool{}
		for _, mode := range m.Service.Interactions {
			if !mode.Valid() || seen[mode] {
				return fmt.Errorf("invalid or repeated service interaction %q", mode)
			}
			seen[mode] = true
		}
	}
	if m.Service != nil && m.Service.Output != nil {
		o := *m.Service.Output
		if o.Save != nil || o.Tail != nil {
			return errors.New("the output of the service cannot save or tail")
		}
		noNames := func(t string) error {
			_, err := template.Compile(t, func(string) bool { return false })
			return err
		}
		if err := m.checkOutput(o, false, noNames); err != nil {
			return fmt.Errorf("the output of the service: %w", err)
		}
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
		if err := checkReserved(name, kind); err != nil {
			return err
		}
		seen := map[string]bool{}
		for _, field := range kind.Frontmatter {
			if !namePattern.MatchString(field) || seen[field] {
				return fmt.Errorf("kind %s: invalid or repeated frontmatter field %q", name, field)
			}
			seen[field] = true
		}
		if kind.NotebookIndex != "" {
			index, ok := m.Kinds[kind.NotebookIndex]
			if !ok || kind.NotebookIndex == name || index.Visibility != "sealed" || index.NotebookIndex != "" || kind.Visibility != "sealed" {
				return fmt.Errorf("kind %s: notebook_index must name a separate sealed index kind", name)
			}
			for _, field := range []string{"title", "page", "notebook"} {
				if !slices.Contains(kind.Frontmatter, field) {
					return fmt.Errorf("kind %s: a notebook requires frontmatter field %s", name, field)
				}
			}
		}
		if kind.NotebookEntry != "" {
			entry, ok := m.Kinds[kind.NotebookEntry]
			if !ok || kind.NotebookEntry == name || kind.NotebookIndex == "" || entry.Visibility != "sealed" || entry.NotebookIndex != "" || entry.NotebookEntry != "" || len(entry.Frontmatter) > 0 {
				return fmt.Errorf("kind %s: notebook_entry must name a separate sealed kind with no frontmatter, of a notebook kind", name)
			}
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
		for _, tmpl := range []string{format.Header, format.Record, format.Empty} {
			if _, err := template.Compile(tmpl, nil); err != nil {
				return fmt.Errorf("format %s: %w", name, err)
			}
		}
	}

	if len(m.Commands) == 0 && (m.Service == nil || (!session.Supports(m.Service.Interactions, session.ServerStream) && !session.Supports(m.Service.Interactions, session.Duplex))) {
		return errors.New("the manifest has no command")
	}
	paths := map[string]bool{}
	for _, c := range m.Commands {
		if c.Action.Call != nil && m.Service != nil && !session.Supports(m.Service.Interactions, session.RequestReply) {
			return errors.New("a call command requires the request_reply interaction")
		}
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
		if a.Name == "json" || a.Name == "later" || a.Name == "help" || a.Name == "dry_run" {
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

	check := func(tmpl string) error {
		_, err := template.Compile(tmpl, func(root string) bool {
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
		if a.Publish.Body != "" {
			name := bodyArg(a.Publish)
			if m.Kinds[a.Publish.Kind].Visibility != "sealed" {
				return errors.New("only a publish of a sealed kind has a body")
			}
			if !slices.ContainsFunc(c.Args, func(arg Arg) bool { return arg.Name == name && arg.Type == "file" }) {
				return fmt.Errorf("the body %q must name one file argument, as {{file}}", a.Publish.Body)
			}
			if a.Publish.Revise == "append" {
				return errors.New("a publish with a body replaces the draft: it cannot append")
			}
		}
		if a.Publish.FrontmatterAddress != "" {
			required := m.Kinds[a.Publish.Kind].Frontmatter
			if !slices.Contains(required, "page") || !slices.Contains(required, "notebook") {
				return errors.New("frontmatter_address requires page and notebook fields")
			}
		}
		templates := append([]string{a.Publish.D, a.Publish.Content.Text, a.Publish.FrontmatterAddress}, a.Publish.To...)
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
	if a.Notebook != nil {
		actions++
		// The d template of a notebook action names the page address, which
		// the notebook resolves, as address.
		checkD := func(tmpl string) error {
			_, err := template.Compile(tmpl, func(root string) bool {
				return root == "address" || slices.Contains(names, root)
			})
			return err
		}
		if err := m.checkNotebook(a.Notebook, check, checkD); err != nil {
			return err
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

	if c.Output.Save != nil {
		if _, err := saveArgument(c); err != nil {
			return err
		}
	}
	return m.checkOutput(c.Output, a.Watch != nil, check)
}

// A save destination is exactly one explicit path argument. Templates of
// response fields, defaults and transformations cannot authorize file writes.
func saveArgument(c Command) (string, error) {
	t, err := template.Compile(c.Output.Save.To, nil)
	if err != nil {
		return "", errors.New("save.to must name one explicit path argument without filters or a default")
	}
	if name, ok := t.PlaceholderOnly(); ok {
		for _, a := range c.Args {
			if a.Name == name && a.Type == "path" && a.Kind != "switch" && a.Default == "" {
				return a.Name, nil
			}
		}
	}
	return "", errors.New("save.to must name one explicit path argument without filters or a default")
}

// checkOutput checks an output pipeline. A watch may tail, and may not
// exit. check checks a template against the names that it may use.
func (m *Manifest) checkOutput(o Output, watch bool, check func(string) error) error {
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
			if _, err := template.Compile(t, nil); err != nil {
				return err
			}
		}
	}
	if o.Tail != nil && !watch {
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
	if len(o.Exit) > 0 && watch {
		return errors.New("exit works only for a command that ends")
	}
	for _, rule := range o.Exit {
		if rule.Code == "" {
			return errors.New("an exit rule needs a code")
		}
		if _, err := template.Compile(rule.Code, nil); err != nil {
			return err
		}
		if err := conds(rule.Where); err != nil {
			return err
		}
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
