package template

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// A Template is text with placeholders: {{name}}, or {{name|filter|filter}}.
type Template struct {
	parts []part
}

// part is literal text, or one placeholder.
type part struct {
	text    string
	name    string
	filters []filter
}

type filter struct {
	name string
	arg  string
}

var placeholderName = regexp.MustCompile(`^[a-z_][a-z0-9_]*(\.[a-z0-9_]+)*(\+[a-z_][a-z0-9_]*(\.[a-z0-9_]+)*)*$`)

// filters are the filters that a template can use, and whether each one
// takes an argument.
var filters = map[string]bool{
	"json": false, "hex": false, "join": false, "event_author": false,
	"keyed": true, "default": true,
	"time": false, "date": false, "name": false, "npub": false, "nevent": false,
	"short": false, "indent": false, "truncate": true,
}

// Compile reads a template. When known is not nil, each placeholder must
// start with a name that known accepts.
func Compile(text string, known func(string) bool) (*Template, error) {
	t := &Template{}
	for text != "" {
		start := strings.Index(text, "{{")
		if start < 0 {
			t.parts = append(t.parts, part{text: text})
			break
		}
		if start > 0 {
			t.parts = append(t.parts, part{text: text[:start]})
		}
		end := strings.Index(text[start:], "}}")
		if end < 0 {
			return nil, fmt.Errorf("the template %q opens {{ and does not close it", text)
		}
		p, err := placeholder(text[start+2 : start+end])
		if err != nil {
			return nil, err
		}
		for name := range strings.SplitSeq(p.name, "+") {
			if known != nil && !known(strings.SplitN(name, ".", 2)[0]) {
				return nil, fmt.Errorf("the template names %q, which is not an argument", name)
			}
		}
		t.parts = append(t.parts, p)
		text = text[start+end+2:]
	}
	return t, nil
}

func placeholder(expr string) (part, error) {
	words := strings.Split(expr, "|")
	p := part{name: strings.TrimSpace(words[0])}
	if !placeholderName.MatchString(p.name) {
		return part{}, fmt.Errorf("{{%s}} does not name a value", expr)
	}
	for _, word := range words[1:] {
		name, arg, hasArg := strings.Cut(strings.TrimSpace(word), ":")
		takes, ok := filters[name]
		if !ok {
			return part{}, fmt.Errorf("the filter %q does not exist", name)
		}
		if takes != hasArg {
			if takes {
				return part{}, fmt.Errorf("the filter %q needs an argument, as %s:<value>", name, name)
			}
			return part{}, fmt.Errorf("the filter %q takes no argument", name)
		}
		if name == "truncate" {
			if n, err := strconv.Atoi(arg); err != nil || n < 1 {
				return part{}, fmt.Errorf("truncate needs a positive number, not %q", arg)
			}
		}
		p.filters = append(p.filters, filter{name: name, arg: arg})
	}
	return p, nil
}

// PlaceholderOnly gives the name of the one placeholder of a template
// that is nothing else: no text, no filters.
func (t *Template) PlaceholderOnly() (string, bool) {
	if len(t.parts) == 1 && t.parts[0].name != "" && len(t.parts[0].filters) == 0 {
		return t.parts[0].name, true
	}
	return "", false
}

// Helpers are what some filters need from outside the template.
type Helpers interface {
	Keyed(purpose, input string) (string, error)
	EventAuthor(ref string) (string, error)
	Name(key string) string
	Sanitize(text string) string
}

// Scope finds the value of a name.
type Scope func(name string) (any, bool)

// Lookup finds a dotted name: the root in the scope, then each field.
func (s Scope) Lookup(name string) any {
	words := strings.Split(name, ".")
	value, ok := s(words[0])
	if !ok {
		return nil
	}
	for _, word := range words[1:] {
		switch v := value.(type) {
		case map[string]any:
			value = v[word]
		case []any:
			i, err := strconv.Atoi(word)
			if err != nil || i < 0 || i >= len(v) {
				return nil
			}
			value = v[i]
		case interface{ Field(string) any }:
			value = v.Field(word)
		default:
			return nil
		}
	}
	return value
}

// Render writes the template as text. With clean, it cleans each part,
// and the result can be shown. Without clean, the result is data, such as
// the content of an event: a caller that shows it must clean it first
// (docs/clients/SPEC.md, section 3.9).
func (t *Template) Render(s Scope, h Helpers, clean bool) (string, error) {
	var out strings.Builder
	for _, p := range t.parts {
		if p.name == "" {
			// The author of the app wrote this text: it is another
			// party's text too (docs/clients/SPEC.md, section 3.9).
			text := p.text
			if clean {
				text = h.Sanitize(text)
			}
			out.WriteString(text) //arc:raw-ok render cleans each part that it shows; a template that builds an event stays as it is
			continue
		}
		value, err := p.value(s, h)
		if err != nil {
			return "", err
		}
		text := Str(value)
		if clean {
			text = h.Sanitize(text)
		}
		out.WriteString(text) //arc:raw-ok render cleans each part that it shows; a template that builds an event stays as it is
	}
	return out.String(), nil
}

func (p part) value(s Scope, h Helpers) (any, error) {
	var value any
	if names := strings.Split(p.name, "+"); len(names) > 1 {
		// {{a+b}} is the list of the values of a and b.
		list := make([]string, len(names))
		for i, name := range names {
			list[i] = Str(s.Lookup(name))
		}
		value = list
	} else {
		value = s.Lookup(p.name)
	}
	for _, f := range p.filters {
		var err error
		if value, err = apply(f, value, h); err != nil {
			return nil, fmt.Errorf("{{%s}}: %w", p.name, err)
		}
	}
	return value, nil
}

func apply(f filter, value any, h Helpers) (any, error) {
	switch f.name {
	case "json":
		body, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		return string(body), nil
	case "hex":
		if value == nil {
			return nil, nil
		}
		return hex.EncodeToString([]byte(Str(value))), nil
	case "join":
		if value == nil {
			return nil, nil
		}
		return Str(value), nil
	case "default":
		if Str(value) == "" {
			return f.arg, nil
		}
		return value, nil
	case "keyed":
		if value == nil {
			return nil, nil
		}
		if list, ok := value.([]string); ok {
			// The words of a list are keyed apart, so a+b and ab differ.
			return h.Keyed(f.arg, strings.Join(list, "\x00"))
		}
		return h.Keyed(f.arg, Str(value))
	case "event_author":
		if value == nil {
			return nil, nil
		}
		return h.EventAuthor(Str(value))
	case "time", "date":
		seconds, err := strconv.ParseInt(Str(value), 10, 64)
		if err != nil {
			return value, nil
		}
		layout := "2006-01-02 15:04"
		if f.name == "date" {
			layout = "2006-01-02"
		}
		return time.Unix(seconds, 0).Local().Format(layout), nil
	case "name":
		if pk, err := nostr.PubKeyFromHex(Str(value)); err == nil {
			return h.Name(pk.Hex()), nil
		}
		return value, nil
	case "npub":
		if pk, err := nostr.PubKeyFromHex(Str(value)); err == nil {
			return nip19.EncodeNpub(pk), nil
		}
		return value, nil
	case "nevent":
		if id, err := nostr.IDFromHex(Str(value)); err == nil {
			return nip19.EncodeNevent(id, nil, nostr.ZeroPK), nil
		}
		return value, nil
	case "short":
		text := Str(value)
		if len(text) > 12 {
			return text[:8] + "…" + text[len(text)-4:], nil
		}
		return text, nil
	case "indent":
		text := Str(value)
		if text == "" {
			return text, nil
		}
		return "  " + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n  "), nil
	case "truncate":
		n, _ := strconv.Atoi(f.arg)
		text := Str(value)
		if utf8.RuneCountInString(text) <= n {
			return text, nil
		}
		runes := []rune(text)
		return string(runes[:n]) + "…", nil
	}
	return nil, errors.New("the filter " + f.name + " does not exist")
}

// Str writes a value as text. An absent value is empty, and a list joins its
// words with one space.
func Str(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case []string:
		return strings.Join(v, " ")
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(v)
	case json.Number:
		return v.String()
	case fmt.Stringer:
		return v.String()
	case []any:
		words := make([]string, len(v))
		for i, item := range v {
			words[i] = Str(item)
		}
		return strings.Join(words, " ")
	}
	body, _ := json.Marshal(value)
	return string(body)
}
