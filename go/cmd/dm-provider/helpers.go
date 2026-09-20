package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// text is one reply with no events.
func text(reply string) *answer { return &answer{reply: reply} }

// lines joins rows, or gives the empty answer when there are none.
func lines(rows []string, empty string) *answer {
	if len(rows) == 0 {
		return text(empty)
	}
	return text(strings.Join(rows, "\n"))
}

// line is one row of the inbox and of a thread.
func line(msg *message, from string) string {
	direction := "in"
	if msg.From == from {
		direction = "out"
	}
	return strings.Join([]string{
		msg.ID, direction, other(msg, from), msg.T, strconv.Itoa(len(msg.Body)),
	}, "\t")
}

// render writes one message for a person to read.
func render(msg *message, retracted bool, reactions []reaction) string {
	rows := []string{
		"id: " + msg.ID,
		"from: " + msg.From,
		"to: " + strings.Join(msg.To, ", "),
		"t: " + msg.T,
	}

	if msg.ReplyTo != "" {
		rows = append(rows, "reply_to: "+msg.ReplyTo)
	}
	if retracted {
		rows = append(rows, "retracted: yes")
	}
	if len(msg.Attachments) > 0 {
		var parts []string
		for _, one := range msg.Attachments {
			parts = append(parts, fmt.Sprintf("%s (%d bytes)", one.Name, one.Bytes))
		}
		rows = append(rows, "attachments: "+strings.Join(parts, ", "))
	}
	if len(reactions) > 0 {
		var parts []string
		for _, one := range reactions {
			parts = append(parts, one.Value+" "+one.By)
		}
		rows = append(rows, "reactions: "+strings.Join(parts, " "))
	}

	return strings.Join(append(rows, "body: "+msg.Body), "\n")
}

// flagsField is "<state>[;reaction=<value>:<by>,...][;attach=<name>:<bytes>,...]".
func flagsField(state string, reactions []reaction, attachments []attach) string {
	out := state

	if len(reactions) > 0 {
		var parts []string
		for _, one := range reactions {
			parts = append(parts, one.Value+":"+one.By)
		}
		out += ";reaction=" + strings.Join(parts, ",")
	}

	if len(attachments) > 0 {
		var parts []string
		for _, one := range attachments {
			parts = append(parts, fmt.Sprintf("%s:%d", one.Name, one.Bytes))
		}
		out += ";attach=" + strings.Join(parts, ",")
	}
	return out
}

// holdersOf is every citizen that holds a copy of a message.
func holdersOf(msg *message) []string {
	return unique(append(append([]string{}, msg.To...), msg.From))
}

// other is the conversation key of a message: every other citizen, sorted
// and joined by commas. A message to yourself alone keys on yourself.
func other(msg *message, from string) string {
	var others []string
	for _, holder := range holdersOf(msg) {
		if holder != from {
			others = append(others, holder)
		}
	}
	if len(others) == 0 {
		return from
	}

	sort.Strings(others)
	return strings.Join(others, ",")
}

// fanOut builds one event for every holder but the caller. The meta always
// names the sender, and, as "to", the conversation key that the recipient
// can open.
func fanOut(holders []string, from, topic string, meta map[string]any, body func(string) string) []event {
	var events []event

	for _, holder := range holders {
		if holder == from {
			continue
		}

		var others []string
		for _, one := range holders {
			if one != holder {
				others = append(others, one)
			}
		}
		sort.Strings(others)

		fields := map[string]any{"from": from, "to": strings.Join(others, ",")}
		for name, value := range meta {
			fields[name] = value
		}

		line := ""
		if body != nil {
			line = body(holder)
		}
		events = append(events, event{To: holder, Topic: topic, Meta: fields, Body: line})
	}
	return events
}

// recipientsOf reads the --to option: one key, or keys joined by commas.
func recipientsOf(value any) ([]string, error) {
	to, ok := value.(string)
	if !ok || to == "" {
		return nil, errorf("invalid_address missing recipient")
	}

	var peers []string
	for _, one := range strings.Split(to, ",") {
		if one = strings.TrimSpace(one); one != "" {
			peers = append(peers, one)
		}
	}
	peers = unique(peers)

	if len(peers) == 0 {
		return nil, errorf("invalid_address missing recipient")
	}
	for _, peer := range peers {
		if !isPublicKey(peer) {
			return nil, errorf("invalid_address")
		}
	}
	return peers, nil
}

// conversationKey turns a set of keys into the form that other builds.
func conversationKey(peer string) (string, error) {
	keys, err := recipientsOf(peer)
	if err != nil {
		return "", err
	}

	sort.Strings(keys)
	return strings.Join(keys, ","), nil
}

// validReaction holds a reaction to one to four characters, with no
// separator.
func validReaction(value string) error {
	if value == "none" {
		return nil
	}

	count := utf8.RuneCountInString(value)
	if count < 1 || count > 4 || strings.ContainsAny(value, " \t\n,;:") {
		return errorf("invalid_reaction one to four characters, no separators")
	}
	return nil
}

// since drops the messages at or below the id of --since.
func since(messages []*message, options map[string]any) []*message {
	id, ok := option(options, "since")
	if !ok {
		return messages
	}

	var found []*message
	for _, msg := range messages {
		if msg.ID > id {
			found = append(found, msg)
		}
	}
	return found
}

// lastOf keeps the last messages, as --limit says. The default is 50.
func lastOf(messages []*message, options map[string]any) []*message {
	count := limitOf(options)
	if len(messages) <= count {
		return messages
	}
	return messages[len(messages)-count:]
}

// limitFirst is how many rows stand at the front of a list.
func limitFirst(size int, options map[string]any) int {
	if count := limitOf(options); size > count {
		return count
	}
	return size
}

func limitOf(options map[string]any) int {
	text, ok := option(options, "limit")
	if !ok {
		return 50
	}

	count, err := strconv.Atoi(text)
	if err != nil || count <= 0 {
		return 50
	}
	return count
}

func hasEvent(index map[string]map[string]bool, id, event string) bool {
	return index[id][event]
}

func unique(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))

	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func without(values []string, drop string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != drop {
			out = append(out, value)
		}
	}
	return out
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func help() string {
	return strings.TrimRight(`dm commands
  conversations [--limit n]
  send --to <hex,...> [--reply-to id]   body: one token per recipient, then yours,
                                        then attach:<name>:<token> lines
  fetch <id> <name>               the sealed attachment token; a name never starts with a dot
  quota                           bytes used of the mailbox budget
  purge --before <id>             delete your own messages older than id
  inbox [--unread] [--since id] [--limit n]
  thread <peer[,peer...]> [--since id] [--limit n] [--bodies]   a group key as conversations prints it; --bodies marks inbound read
  read <id>
  ack <id>...
  status <id>
  archive <id>...
  react <id> <emoji|none>
  retract <id>                    sender only, within the retract window; emits dm.retracted
  block <peer> | unblock <peer> | blocked
  mute <peer> | unmute <peer> | muted
  settings [receipts on|off]
  whoami
`, "\n")
}
