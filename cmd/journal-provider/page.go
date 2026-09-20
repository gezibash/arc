// Command journal-provider keeps markdown notebooks for citizens.
//
// A page is YAML frontmatter and a markdown body. Every write is a git
// commit, attributed to the public key of the caller.
package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// page is the frontmatter and the body of one page.
type page struct {
	Meta map[string]any
	Body string
}

// metaOrder is the order that the frontmatter of a page follows. A key that
// is not named here comes after, in byte order.
var metaOrder = []string{"title", "created", "author", "updated", "tags", "refs", "attachments", "links"}

var plainScalar = regexp.MustCompile(`^[A-Za-z0-9_./:@+-]+$`)

// parsePage reads the frontmatter and the body. Text without frontmatter is
// all body.
func parsePage(text string) *page {
	rest, found := strings.CutPrefix(text, "---\n")
	if !found {
		return &page{Meta: map[string]any{}, Body: text}
	}

	front, body, found := strings.Cut(rest, "\n---\n")
	if !found {
		return &page{Meta: map[string]any{}, Body: text}
	}

	meta := map[string]any{}
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil || meta == nil {
		meta = map[string]any{}
	}
	return &page{Meta: meta, Body: body}
}

// render writes the page back, frontmatter first.
func (p *page) render() string {
	return "---\n" + encodeYAML(p.Meta) + "---\n" + p.Body
}

// encodeYAML writes the small set of shapes that the journal uses. The
// journal writes its own YAML so that a page is the same on every machine.
func encodeYAML(meta map[string]any) string {
	var keys []string
	for _, key := range metaOrder {
		if _, held := meta[key]; held {
			keys = append(keys, key)
		}
	}

	var rest []string
	for key := range meta {
		if !inOrder(key) {
			rest = append(rest, key)
		}
	}
	sort.Strings(rest)

	var out strings.Builder
	for _, key := range append(keys, rest...) {
		out.WriteString(encodeField(key, meta[key], 0))
	}
	return out.String()
}

func encodeField(key string, value any, indent int) string {
	pad := strings.Repeat(" ", indent)

	list, isList := asList(value)
	if !isList {
		return fmt.Sprintf("%s%s: %s\n", pad, key, scalar(value))
	}
	if len(list) == 0 {
		return fmt.Sprintf("%s%s: []\n", pad, key)
	}

	var out strings.Builder
	out.WriteString(pad + key + ":\n")
	for _, item := range list {
		out.WriteString(encodeItem(item, indent+2))
	}
	return out.String()
}

func encodeItem(value any, indent int) string {
	pad := strings.Repeat(" ", indent)

	fields, isMap := asMap(value)
	if !isMap {
		return fmt.Sprintf("%s- %s\n", pad, scalar(value))
	}

	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)

	var out strings.Builder
	for index, name := range names {
		prefix := pad + "  "
		if index == 0 {
			prefix = pad + "- "
		}
		out.WriteString(fmt.Sprintf("%s%s: %s\n", prefix, name, scalar(fields[name])))
	}
	return out.String()
}

// scalar writes one value. A plain word stands as it is, and anything else
// takes quotes, as JSON writes them.
func scalar(value any) string {
	switch value := value.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("%t", value)
	case int:
		return fmt.Sprintf("%d", value)
	case int64:
		return fmt.Sprintf("%d", value)
	case float64:
		if value == float64(int64(value)) {
			return fmt.Sprintf("%d", int64(value))
		}
		return fmt.Sprintf("%g", value)
	case time.Time:
		// The YAML reader turns a time into a value of its own. It goes back
		// as the text that the journal wrote.
		return value.UTC().Format("2006-01-02T15:04:05Z")
	case string:
		if value != "" && plainScalar.MatchString(value) && !strings.HasPrefix(value, "-") {
			return value
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return `""`
		}
		return string(encoded)
	default:
		encoded, err := json.Marshal(fmt.Sprint(value))
		if err != nil {
			return `""`
		}
		return string(encoded)
	}
}

func inOrder(key string) bool {
	for _, name := range metaOrder {
		if name == key {
			return true
		}
	}
	return false
}

func asList(value any) ([]any, bool) {
	switch value := value.(type) {
	case []any:
		return value, true
	case []string:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, item)
		}
		return out, true
	case []map[string]any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, item)
		}
		return out, true
	default:
		return nil, false
	}
}

func asMap(value any) (map[string]any, bool) {
	switch value := value.(type) {
	case map[string]any:
		return value, true
	case map[any]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[fmt.Sprint(key)] = item
		}
		return out, true
	default:
		return nil, false
	}
}
