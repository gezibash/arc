package iface

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// A template is text with placeholders: {{name}}, or {{name|filter|filter}}.
type template struct {
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

// compile reads a template. When known is not nil, each placeholder must
// start with a name that known accepts.
func compile(text string, known func(string) bool) (*template, error) {
	t := &template{}
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
		for _, name := range strings.Split(p.name, "+") {
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

// helpers are what some filters need from outside the template.
type helpers interface {
	keyed(purpose, input string) (string, error)
	eventAuthor(ref string) (string, error)
	name(key string) string
}

// scope finds the value of a name.
type scope func(name string) (any, bool)

// lookup finds a dotted name: the root in the scope, then each field.
func (s scope) lookup(name string) any {
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
		case interface{ field(string) any }:
			value = v.field(word)
		default:
			return nil
		}
	}
	return value
}

// render writes the template as text.
func (t *template) render(s scope, h helpers, clean bool) (string, error) {
	var out strings.Builder
	for _, p := range t.parts {
		if p.name == "" {
			out.WriteString(p.text)
			continue
		}
		value, err := p.value(s, h)
		if err != nil {
			return "", err
		}
		text := str(value)
		if clean {
			text = sanitize(text)
		}
		out.WriteString(text)
	}
	return out.String(), nil
}

// value renders a template that is one placeholder as its value, so a list
// stays a list. Other templates render as text.
func (t *template) value(s scope, h helpers) (any, error) {
	if len(t.parts) == 1 && t.parts[0].name != "" {
		return t.parts[0].value(s, h)
	}
	return t.render(s, h, false)
}

func (p part) value(s scope, h helpers) (any, error) {
	var value any
	if names := strings.Split(p.name, "+"); len(names) > 1 {
		// {{a+b}} is the list of the values of a and b.
		list := make([]string, len(names))
		for i, name := range names {
			list[i] = str(s.lookup(name))
		}
		value = list
	} else {
		value = s.lookup(p.name)
	}
	for _, f := range p.filters {
		var err error
		if value, err = apply(f, value, h); err != nil {
			return nil, fmt.Errorf("{{%s}}: %w", p.name, err)
		}
	}
	return value, nil
}

func apply(f filter, value any, h helpers) (any, error) {
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
		return hex.EncodeToString([]byte(str(value))), nil
	case "join":
		if value == nil {
			return nil, nil
		}
		return str(value), nil
	case "default":
		if str(value) == "" {
			return f.arg, nil
		}
		return value, nil
	case "keyed":
		if value == nil {
			return nil, nil
		}
		if list, ok := value.([]string); ok {
			// The words of a list are keyed apart, so a+b and ab differ.
			return h.keyed(f.arg, strings.Join(list, "\x00"))
		}
		return h.keyed(f.arg, str(value))
	case "event_author":
		if value == nil {
			return nil, nil
		}
		return h.eventAuthor(str(value))
	case "time", "date":
		seconds, err := strconv.ParseInt(str(value), 10, 64)
		if err != nil {
			return value, nil
		}
		layout := "2006-01-02 15:04"
		if f.name == "date" {
			layout = "2006-01-02"
		}
		return time.Unix(seconds, 0).Local().Format(layout), nil
	case "name":
		if pk, err := nostr.PubKeyFromHex(str(value)); err == nil {
			return h.name(pk.Hex()), nil
		}
		return value, nil
	case "npub":
		if pk, err := nostr.PubKeyFromHex(str(value)); err == nil {
			return nip19.EncodeNpub(pk), nil
		}
		return value, nil
	case "nevent":
		if id, err := nostr.IDFromHex(str(value)); err == nil {
			return nip19.EncodeNevent(id, nil, nostr.ZeroPK), nil
		}
		return value, nil
	case "short":
		text := str(value)
		if len(text) > 12 {
			return text[:8] + "…" + text[len(text)-4:], nil
		}
		return text, nil
	case "indent":
		text := str(value)
		if text == "" {
			return text, nil
		}
		return "  " + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n  "), nil
	case "truncate":
		n, _ := strconv.Atoi(f.arg)
		text := str(value)
		if utf8.RuneCountInString(text) <= n {
			return text, nil
		}
		runes := []rune(text)
		return string(runes[:n]) + "…", nil
	}
	return nil, errors.New("the filter " + f.name + " does not exist")
}

// str writes a value as text. An absent value is empty, and a list joins its
// words with one space.
func str(value any) string {
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
			words[i] = str(item)
		}
		return strings.Join(words, " ")
	}
	body, _ := json.Marshal(value)
	return string(body)
}

// sanitize removes every control character except newline and tab, so a
// value from an event cannot move the cursor or change the terminal.
func sanitize(text string) string {
	if !strings.ContainsFunc(text, isControl) {
		return text
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, text)
}

func isControl(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return r < 0x20 || (r >= 0x7f && r < 0xa0) || slices.Contains([]rune{0x2028, 0x2029, 0x200e, 0x200f, 0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2066, 0x2067, 0x2068, 0x2069}, r)
}
