package iface

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
)

// frontmatter reads flat string fields between two delimiter lines. This is
// deliberately a key/value format, not a full YAML parser.
func frontmatter(content string) (map[string]any, string, error) {
	fields := map[string]any{}
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return fields, content, nil
	}
	start := strings.IndexByte(content, '\n') + 1
	offset := start
	for offset < len(content) {
		end := strings.IndexByte(content[offset:], '\n')
		next := len(content)
		if end >= 0 {
			next = offset + end + 1
		}
		line := strings.TrimSuffix(strings.TrimSuffix(content[offset:next], "\n"), "\r")
		if next > 4096 {
			return nil, "", fmt.Errorf("frontmatter: header exceeds 4096 bytes")
		}
		if line == "---" {
			return fields, content[next:], nil
		}
		key, value, ok := strings.Cut(line, ":")
		if !ok || !namePattern.MatchString(key) {
			return nil, "", fmt.Errorf("frontmatter: expected a flat key: value field")
		}
		if _, exists := fields[key]; exists {
			return nil, "", fmt.Errorf("frontmatter: duplicate field %q", key)
		}
		value = strings.TrimSpace(value)
		if strings.HasPrefix(value, "\"") {
			var err error
			value, err = strconv.Unquote(value)
			if err != nil {
				return nil, "", fmt.Errorf("frontmatter: field %q has invalid quotes", key)
			}
		} else if strings.HasPrefix(value, "'") {
			if len(value) < 2 || !strings.HasSuffix(value, "'") {
				return nil, "", fmt.Errorf("frontmatter: field %q has invalid quotes", key)
			}
			value = strings.ReplaceAll(value[1:len(value)-1], "''", "'")
		} else if strings.ContainsAny(value, "\r\n") || strings.HasPrefix(value, "[") || strings.HasPrefix(value, "{") || value == "|" || value == ">" {
			return nil, "", fmt.Errorf("frontmatter: field %q must be a flat string", key)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return nil, "", fmt.Errorf("frontmatter: field %q must be one line", key)
		}
		fields[key] = value
		offset = next
	}
	return nil, "", fmt.Errorf("frontmatter: missing closing --- delimiter")
}

// enforceFrontmatter runs before any event is signed or stored. Reads do not
// apply a new manifest's write policy to historical pages.
func (r *run) enforceFrontmatter(p *Publish, content string, tags nostr.Tags) (nostr.Tags, error) {
	required := r.kindOf(p.Kind).Frontmatter
	if len(required) == 0 {
		return tags, nil
	}
	fields, _, err := frontmatter(content)
	if err != nil {
		return nil, err
	}
	for _, key := range required {
		value, _ := fields[key].(string)
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("frontmatter: requires nonempty %q between opening and closing --- lines", key)
		}
	}
	if p.FrontmatterAddress != "" {
		address, err := r.template(p.FrontmatterAddress)
		if err != nil {
			return nil, err
		}
		notebook, _ := fields["notebook"].(string)
		page, _ := fields["page"].(string)
		if notebook+"/"+page != address {
			return nil, fmt.Errorf("frontmatter: notebook/page must match address %q", address)
		}
	}
	if title, ok := fields["title"].(string); ok {
		for _, tag := range tags {
			if len(tag) > 1 && tag[0] == "title" && tag[1] != title {
				return nil, fmt.Errorf("frontmatter: title conflicts with --title or the existing page title")
			}
		}
		tags = slices.DeleteFunc(tags, func(tag nostr.Tag) bool { return len(tag) > 0 && tag[0] == "title" })
		tags = append(tags, nostr.Tag{"title", title})
	}
	return tags, nil
}
