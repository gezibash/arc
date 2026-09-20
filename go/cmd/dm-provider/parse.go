package main

import (
	"encoding/json"
	"regexp"
	"strings"
)

// The parser reads one command line into positionals and options.
//
// An option is "--flag value", "--flag \"value\"", or a bare "--flag".
//
// A quoted value that is a JSON string literal, as the {{key|json}} filter of
// the CLI writes, is decoded. Quotes, newlines and " --words" inside a
// --body therefore survive. Any other quoted value runs to the last quote
// before the next " --flag", so quotes that a person typed survive as well.
//
// The positionals end at the first "--flag" that stands at a token boundary.
//
// An empty value, the literal null, and the literal false count as absent.
// The literal true reads as a bare flag.
//
// The scanner walks the line once, so a body of several megabytes parses in
// one pass.

var (
	flagPattern       = regexp.MustCompile(`(^|\s)--([a-z][a-z0-9-]*)`)
	afterQuotePattern = regexp.MustCompile(`^(\s+--[a-z][a-z0-9-]*(\s|$)|\s*$)`)
	afterTokenPattern = regexp.MustCompile(`^(\s|$)`)
	spacePattern      = regexp.MustCompile(`^\s+`)
	barePattern       = regexp.MustCompile(`^\S+`)
	tokenPattern      = regexp.MustCompile(`^("((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+))`)
)

// splitMessage splits a request into its command line and its body. The body
// is everything after the first newline.
func splitMessage(message string) (line string, body string, hasBody bool) {
	if index := strings.Index(message, "\n"); index >= 0 {
		return message[:index], message[index+1:], true
	}
	return message, "", false
}

// parseLine reads one command line.
func parseLine(line string) ([]string, map[string]any) {
	args, rest := positionals(strings.TrimSpace(line), nil)
	return args, scanOptions(rest, map[string]any{})
}

// positionals runs from the start of the line to the first flag that stands
// at a token boundary.
func positionals(text string, found []string) ([]string, string) {
	for {
		text = strings.TrimLeft(text, " \t\n\r")

		switch {
		case text == "":
			return found, ""

		case strings.HasPrefix(text, "--") && len(text) > 2 && text[2] >= 'a' && text[2] <= 'z':
			return found, text

		case strings.HasPrefix(text, `"`):
			if value, rest, ok := jsonString(text, afterTokenPattern); ok {
				found = append(found, value)
				text = rest
				continue
			}
			fallthrough

		default:
			value, rest := shellToken(text)
			found = append(found, value)
			text = rest
		}
	}
}

// shellToken reads one token: a double quoted value, a single quoted value,
// or a run without space.
func shellToken(text string) (string, string) {
	match := tokenPattern.FindStringSubmatchIndex(text)
	if match == nil {
		return "", ""
	}

	groups := tokenPattern.FindStringSubmatch(text)
	value := ""

	switch {
	case groups[2] != "":
		value = unescape(groups[2])
	case groups[3] != "":
		value = groups[3]
	default:
		value = groups[4]
	}
	return value, text[match[1]:]
}

func scanOptions(text string, found map[string]any) map[string]any {
	for {
		match := flagPattern.FindStringSubmatchIndex(text)
		if match == nil {
			return found
		}

		name := text[match[4]:match[5]]
		value, rest := takeValue(text[match[5]:])
		putOption(found, name, value)
		text = rest
	}
}

// value is what follows a flag: nothing, a quoted run, or one bare token.
type value struct {
	text   string
	quoted bool
	absent bool
}

// takeValue reads the value of one flag. The value follows at least one
// space.
func takeValue(rest string) (value, string) {
	space := spacePattern.FindString(rest)
	if space == "" {
		return value{absent: true}, rest
	}

	text := rest[len(space):]

	switch {
	case text == "", strings.HasPrefix(text, "--"):
		return value{absent: true}, rest

	case strings.HasPrefix(text, `"`):
		if found, after, ok := jsonString(text, afterQuotePattern); ok {
			return value{text: found, quoted: true}, after
		}
		if close, ok := closeQuote(text); ok {
			return value{text: text[1:close], quoted: true}, text[close+1:]
		}
		return bareValue(text, rest)

	default:
		return bareValue(text, rest)
	}
}

func bareValue(text, rest string) (value, string) {
	found := barePattern.FindString(text)
	if found == "" {
		return value{absent: true}, rest
	}
	return value{text: found}, text[len(found):]
}

// jsonString reads a JSON string literal that ends at a boundary.
func jsonString(text string, boundary *regexp.Regexp) (string, string, bool) {
	length, ok := jsonStringLength(text)
	if !ok {
		return "", "", false
	}

	after := text[length:]
	if !boundary.MatchString(after) {
		return "", "", false
	}

	var found string
	if err := json.Unmarshal([]byte(text[:length]), &found); err != nil {
		return "", "", false
	}
	return found, after, true
}

// jsonStringLength is the length of the JSON string literal at the front,
// quotes included. Escapes and quotes are ASCII, so reading bytes is safe.
func jsonStringLength(text string) (int, bool) {
	if !strings.HasPrefix(text, `"`) {
		return 0, false
	}

	for index := 1; index < len(text); {
		switch text[index] {
		case '\\':
			index += 2
		case '"':
			return index + 1, true
		default:
			index++
		}
	}
	return 0, false
}

// closeQuote finds the quote that a flag or the end of the line follows.
func closeQuote(text string) (int, bool) {
	for index := 1; index < len(text); index++ {
		if text[index] == '"' && afterQuotePattern.MatchString(text[index+1:]) {
			return index, true
		}
	}
	return 0, false
}

func putOption(found map[string]any, name string, given value) {
	key := strings.ReplaceAll(name, "-", "_")

	switch {
	case given.absent:
		found[key] = true
	case given.text == "" && given.quoted:
		return
	case given.text == "false":
		return
	case given.text == "true":
		found[key] = true
	case given.text == "null" && !given.quoted:
		return
	default:
		found[key] = given.text
	}
}

// unescape drops the backslash of every escaped character.
func unescape(text string) string {
	var out strings.Builder
	for index := 0; index < len(text); index++ {
		if text[index] == '\\' && index+1 < len(text) {
			index++
		}
		out.WriteByte(text[index])
	}
	return out.String()
}

// option reads one option as a string. A bare flag is not a string.
func option(options map[string]any, name string) (string, bool) {
	text, ok := options[name].(string)
	return text, ok
}

// flagSet says whether a bare flag stands.
func flagSet(options map[string]any, name string) bool {
	set, ok := options[name].(bool)
	return ok && set
}
