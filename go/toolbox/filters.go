package toolbox

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gezibash/arc/go/identity"
	"github.com/gezibash/arc/go/sealedbox"
)

// A capability may say how its answers are shown. The filters run in the
// order that the capability names.
//
//	open         open every sealed token with the key of the caller
//	petnames     write a public key as the name of its citizen
//	preview:n    cut the last field of each record to n characters
//	conversation show a thread as people read it
//	markdown     show a thread as a document
//	cache        keep each record on this machine, sealed
var (
	sealedToken = regexp.MustCompile(`sealed-v1:([A-Za-z0-9+/=]+)`)
	hexKey      = regexp.MustCompile(`\b[a-f0-9]{64}\b`)
)

// ApplyOutput runs the filters of a command over its answer. An extra filter
// of the caller, such as the cache, runs by name.
func ApplyOutput(text string, filters []string, me *identity.Identity, extra map[string]func(string) string) string {
	for _, filter := range filters {
		switch {
		case filter == "open" && me != nil:
			text = Open(text, me)
		case filter == "petnames":
			text = Petnames(text)
		case strings.HasPrefix(filter, "preview:"):
			if count, err := strconv.Atoi(strings.TrimPrefix(filter, "preview:")); err == nil && count > 0 {
				text = Preview(text, count)
			}
		case filter == "conversation":
			text = Conversation(text)
		case filter == "markdown":
			text = Markdown(text)
		default:
			if held, ok := extra[filter]; ok {
				text = held(text)
			}
		}
	}
	return text
}

// OutputFilters reads the filters of one command.
func OutputFilters(command map[string]any) []string {
	output, _ := command["output"].(map[string]any)
	if output == nil {
		return nil
	}

	list, _ := output["filters"].([]any)
	filters := make([]string, 0, len(list))

	for _, item := range list {
		if name, ok := item.(string); ok {
			filters = append(filters, name)
		}
	}
	return filters
}

// Open opens every sealed token that the caller can read.
func Open(text string, me *identity.Identity) string {
	return sealedToken.ReplaceAllStringFunc(text, func(token string) string {
		encoded := strings.TrimPrefix(token, "sealed-v1:")

		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return "[sealed: cannot open]"
		}

		plain, err := sealedbox.Open(me, raw)
		if err != nil {
			return "[sealed: cannot open]"
		}
		return string(plain)
	})
}

// Petnames writes every public key as the name of its citizen. A name
// follows from the key, so nothing is looked up.
func Petnames(text string) string {
	return hexKey.ReplaceAllStringFunc(text, func(value string) string {
		raw, err := hex.DecodeString(value)
		if err != nil {
			return value
		}
		return identity.Name(raw)
	})
}

// Preview cuts the last field of each record to a length, on one line. A
// record is a line with tabs, and every line after it without one.
func Preview(text string, length int) string {
	var out []string

	for _, record := range records(strings.Split(text, "\n")) {
		if !strings.Contains(record, "\t") {
			out = append(out, record)
			continue
		}

		fields := strings.Split(record, "\t")
		last := fields[len(fields)-1]
		fields[len(fields)-1] = truncate(strings.ReplaceAll(last, "\n", " "), length)
		out = append(out, strings.Join(fields, "\t"))
	}
	return strings.Join(out, "\n")
}

// Conversation shows a thread as people read it.
func Conversation(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return text
	}

	out := []string{"── " + lines[0] + " ──"}

	for _, record := range records(lines[1:]) {
		held, ok := parseRecord(record)
		if !ok {
			out = append(out, record)
			continue
		}

		who := held.Peer
		switch {
		case held.Direction == "out" && strings.Contains(held.Peer, ","):
			who = "you → " + held.Peer
		case held.Direction == "out":
			who = "you"
		}

		reply := ""
		if held.ReplyTo != "-" && held.ReplyTo != "" {
			reply = "  ↳ reply to " + shortID(held.ReplyTo)
		}

		body := held.Body
		if held.State == "retracted" {
			body = "(retracted)"
		}

		out = append(out, "", fmt.Sprintf("%s  %s  %s%s", who, showTime(held.T), shortID(held.ID), reply))
		for _, line := range strings.Split(strings.TrimRight(body, "\n"), "\n") {
			out = append(out, "  "+line)
		}
		for _, one := range held.Attachments {
			out = append(out, fmt.Sprintf("  📎 %s (%s bytes)", one[0], one[1]))
		}
		if len(held.Reactions) > 0 {
			var parts []string
			for _, one := range held.Reactions {
				parts = append(parts, one[0]+" "+one[1])
			}
			out = append(out, "  "+strings.Join(parts, "  "))
		}
		if held.Direction == "out" && (held.State == "read" || held.State == "delivered") {
			out = append(out, "  ✓ "+held.State)
		}
	}
	return strings.Join(out, "\n")
}

// Markdown shows a thread as a document, for keeping.
func Markdown(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return text
	}

	out := []string{"# " + lines[0], ""}

	for _, record := range records(lines[1:]) {
		held, ok := parseRecord(record)
		if !ok {
			out = append(out, record)
			continue
		}

		who := held.Peer
		if held.Direction == "out" {
			who = "you"
		}

		reply := ""
		if held.ReplyTo != "-" && held.ReplyTo != "" {
			reply = " (reply to " + held.ReplyTo + ")"
		}

		body := strings.TrimRight(held.Body, "\n")
		if held.State == "retracted" {
			body = "_(retracted)_"
		}

		out = append(out,
			fmt.Sprintf("## %s — %s%s", who, held.T, reply), "",
			"<!-- id: "+held.ID+" -->", body, "")

		for _, one := range held.Attachments {
			out = append(out, fmt.Sprintf("- 📎 %s (%s bytes)", one[0], one[1]))
		}
		for _, one := range held.Reactions {
			out = append(out, "- "+one[0]+" "+one[1])
		}
		out = append(out, "")
	}
	return strings.Join(out, "\n")
}

// Record is one message of a thread.
type Record struct {
	ID          string
	Direction   string
	Peer        string
	T           string
	ReplyTo     string
	State       string
	Reactions   [][2]string
	Attachments [][2]string
	Body        string
}

// Records reads the messages of a thread.
func Records(text string) []Record {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return nil
	}

	var out []Record
	for _, record := range records(lines[1:]) {
		if held, ok := parseRecord(record); ok {
			out = append(out, held)
		}
	}
	return out
}

// records folds the lines that follow a record into it. The body of a
// message may run over several lines.
func records(lines []string) []string {
	var out []string

	for _, line := range lines {
		if strings.Contains(line, "\t") || len(out) == 0 {
			out = append(out, line)
			continue
		}
		out[len(out)-1] += "\n" + line
	}
	return out
}

// parseRecord reads one record: id, direction, peer, time, reply, flags and
// body, in that order.
func parseRecord(record string) (Record, bool) {
	fields := strings.SplitN(record, "\t", 7)
	if len(fields) != 7 {
		return Record{}, false
	}

	state, reactions, attachments := parseFlags(fields[5])

	return Record{
		ID: fields[0], Direction: fields[1], Peer: fields[2], T: fields[3],
		ReplyTo: fields[4], State: state, Reactions: reactions,
		Attachments: attachments, Body: fields[6],
	}, true
}

// parseFlags reads "<state>[;reaction=<value>:<by>,...][;attach=<name>:<bytes>,...]".
func parseFlags(flags string) (state string, reactions, attachments [][2]string) {
	parts := strings.Split(flags, ";")
	state = parts[0]

	for _, part := range parts[1:] {
		name, list, found := strings.Cut(part, "=")
		if !found {
			continue
		}

		var pairs [][2]string
		for _, item := range strings.Split(list, ",") {
			first, second, _ := strings.Cut(item, ":")
			pairs = append(pairs, [2]string{first, second})
		}

		switch name {
		case "reaction":
			reactions = pairs
		case "attach":
			attachments = pairs
		}
	}
	return state, reactions, attachments
}

func truncate(value string, length int) string {
	runes := []rune(value)
	if len(runes) <= length {
		return value
	}
	return string(runes[:length-1]) + "…"
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[len(id)-6:]
	}
	return id
}

// showTime writes an ISO time as a date and the hour.
func showTime(value string) string {
	if len(value) >= 16 && value[10] == 'T' {
		return value[:10] + " " + value[11:16]
	}
	return value
}
