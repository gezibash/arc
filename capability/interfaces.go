package capability

import (
	"regexp"
	"strings"
)

// The declarative interfaces of a capability. The CLI of a caller reads them,
// and builds its commands from them. They travel on the wire inside the
// capability, so the shape here must match the shape of every other
// implementation.

// CLIVersion is the interface version of a manifest that names none.
const CLIVersion = 1

// MaxCLIVersion is the newest interface version that this build renders.
const MaxCLIVersion = 4

var (
	segmentSplit  = regexp.MustCompile(`\s+`)
	segmentClean  = regexp.MustCompile(`[^a-z0-9-]+`)
	segmentDashes = regexp.MustCompile(`-+`)
	previewFilter = regexp.MustCompile(`^preview:[1-9][0-9]*$`)
)

var outputFilters = map[string]bool{
	"open": true, "petnames": true, "conversation": true, "markdown": true, "cache": true,
}

// normalizeInterfaces returns the interfaces of a capability, or nil.
func normalizeInterfaces(value any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	cli := normalizeCLI(fields["cli"])
	if cli == nil {
		return nil
	}
	return map[string]any{"cli": cli}
}

// interfacesFromLegacyCLI reads a capability that holds "cli" at its top.
func interfacesFromLegacyCLI(value any) map[string]any {
	cli := normalizeCLI(value)
	if cli == nil {
		return nil
	}
	return map[string]any{"cli": cli}
}

func normalizeCLI(value any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	command, _ := fields["command"].(map[string]any)
	namespace := firstString(fields["namespace"], fields["name"], command["name"])
	summary := firstString(fields["summary"], command["summary"])

	commands := normalizeCommands(fields, summary)
	if namespace == "" || len(commands) == 0 {
		return nil
	}

	cli := map[string]any{
		"version":   normalizeVersion(fields["version"]),
		"namespace": namespace,
		"commands":  commands,
	}
	if summary != "" {
		cli["summary"] = summary
	}
	return cli
}

func normalizeCommands(cli map[string]any, summary string) []any {
	list, ok := cli["commands"].([]any)
	if !ok {
		if command := normalizeLegacyRootCommand(cli, summary); command != nil {
			return []any{command}
		}
		return nil
	}

	out := make([]any, 0, len(list))
	seen := map[string]bool{}

	for _, item := range list {
		command := normalizeCommand(item, summary)
		if command == nil {
			continue
		}

		key := strings.Join(stringList(command["path"]), " ")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, command)
	}
	return out
}

// normalizeLegacyRootCommand reads a CLI that names its one command at the top.
func normalizeLegacyRootCommand(cli map[string]any, summary string) map[string]any {
	args := normalizeArgs(cli["args"])
	input := normalizeInput(cli["input"], args)
	examples := stringList(cli["examples"])

	if len(args) == 0 && input == nil && len(examples) == 0 && summary == "" {
		return nil
	}

	command := map[string]any{"path": []any{}}
	putPresent(command, "summary", summary)
	putList(command, "args", args)
	putMap(command, "input", input)
	putList(command, "examples", anyList(examples))
	return command
}

func normalizeCommand(value any, fallbackSummary string) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	path, ok := commandPath(fields["path"], fields["name"])
	if !ok {
		return nil
	}

	args := normalizeArgs(fields["args"])
	summary := firstString(fields["summary"])
	if summary == "" {
		summary = fallbackSummary
	}

	command := map[string]any{"path": path}
	putPresent(command, "summary", summary)
	putList(command, "args", args)
	putMap(command, "input", normalizeInput(fields["input"], args))
	putMap(command, "invoke", normalizeInvoke(fields["invoke"]))
	putMap(command, "output", normalizeOutput(fields["output"]))
	putList(command, "examples", anyList(stringList(fields["examples"])))
	return command
}

// commandPath turns the path of a command into its segments. A command that
// names no path sits at the root, and holds an empty path. A path that holds
// no usable segment stops the command.
func commandPath(path, name any) ([]any, bool) {
	source := path
	if source == nil {
		source = name
	}

	var raw []string

	switch value := source.(type) {
	case nil:
		return []any{}, true
	case string:
		for _, segment := range segmentSplit.Split(value, -1) {
			if segment != "" {
				raw = append(raw, segment)
			}
		}
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok && text != "" {
				raw = append(raw, text)
			}
		}
	default:
		return nil, false
	}

	if len(raw) == 0 {
		return []any{}, true
	}

	clean := make([]any, 0, len(raw))
	for _, segment := range raw {
		segment = strings.ToLower(segment)
		segment = segmentClean.ReplaceAllString(segment, "-")
		segment = segmentDashes.ReplaceAllString(segment, "-")
		segment = strings.Trim(segment, "-")
		if segment != "" {
			clean = append(clean, segment)
		}
	}

	if len(clean) == 0 {
		return nil, false
	}
	return clean, true
}

func normalizeArgs(value any) []any {
	list, ok := value.([]any)
	if !ok {
		return nil
	}

	args := make([]any, 0, len(list))
	for _, item := range list {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, ok := fields["name"].(string)
		if !ok || name == "" {
			continue
		}

		kind := "positional"
		if fields["kind"] == "option" {
			kind = "option"
		}

		argType := "string"
		if fields["type"] == "boolean" {
			argType = "boolean"
		}

		arg := map[string]any{
			"name":     name,
			"kind":     kind,
			"type":     argType,
			"required": truthy(fields["required"], kind == "positional"),
			"variadic": kind == "positional" && truthy(fields["variadic"], false),
		}
		putPresent(arg, "description", firstString(fields["description"]))
		putPresent(arg, "flag", normalizeFlag(fields["flag"], name, kind))

		args = append(args, arg)
	}
	return variadicTailOnly(args)
}

// variadicTailOnly keeps the variadic mark on the last positional only.
// Options may follow it.
func variadicTailOnly(args []any) []any {
	last := -1
	for index, item := range args {
		if arg, ok := item.(map[string]any); ok && arg["kind"] == "positional" {
			last = index
		}
	}
	if last < 0 {
		return args
	}

	for index, item := range args {
		if arg, ok := item.(map[string]any); ok && index != last {
			arg["variadic"] = false
		}
	}
	return args
}

func normalizeFlag(flag any, name, kind string) string {
	if kind != "option" {
		return ""
	}
	if text, ok := flag.(string); ok && text != "" {
		if strings.HasPrefix(text, "--") {
			return text
		}
		return "--" + text
	}
	return "--" + strings.ReplaceAll(name, "_", "-")
}

// normalizeInput says how the CLI builds the body of a request.
func normalizeInput(value any, args []any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		// One argument and no input means that the argument is the body.
		if len(args) == 1 {
			if arg, ok := args[0].(map[string]any); ok {
				if name, ok := arg["name"].(string); ok {
					return map[string]any{"source": "arg", "name": name, "join_with": " "}
				}
			}
		}
		return nil
	}

	switch fields["source"] {
	case "agora":
		return map[string]any{"source": "agora", "operation": stringOrNil(fields["operation"])}

	case "sealed_file":
		return map[string]any{"source": "sealed_file", "file": stringOrNil(fields["file"])}

	case "private_file":
		return map[string]any{
			"source":    "private_file",
			"operation": stringOrNil(fields["operation"]),
			"id":        stringOrNil(fields["id"]),
			"after":     stringOrNil(fields["after"]),
		}

	case "arg":
		name, ok := fields["name"].(string)
		if !ok || name == "" {
			return nil
		}
		joinWith := " "
		if given, ok := fields["join_with"].(string); ok {
			joinWith = given
		}
		return map[string]any{"source": "arg", "name": name, "join_with": joinWith}

	case "template":
		template, ok := fields["template"].(string)
		if !ok || template == "" {
			return nil
		}
		return map[string]any{"source": "template", "template": template}

	case "json":
		return map[string]any{"source": "json"}

	case "stdin":
		joinWith := "\n"
		if given, ok := fields["join_with"].(string); ok {
			joinWith = given
		}

		input := map[string]any{"source": "stdin", "join_with": joinWith}
		putPresent(input, "template", firstString(fields["template"]))
		putList(input, "seal_to", anyList(sealTo(fields["seal_to"])))
		putPresent(input, "body", firstString(fields["body"]))
		putPresent(input, "file", firstString(fields["file"]))
		putPresent(input, "attach", firstString(fields["attach"]))
		return input

	default:
		return nil
	}
}

func sealTo(value any) []string {
	switch value := value.(type) {
	case string:
		if value != "" {
			return []string{value}
		}
	case []any:
		var out []string
		for _, item := range value {
			if text, ok := item.(string); ok && text != "" {
				out = append(out, text)
			}
		}
		return out
	}
	return nil
}

func normalizeInvoke(value any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	invoke := map[string]any{}
	switch fields["mode"] {
	case "stream", "events", "request_reply":
		invoke["mode"] = fields["mode"]
	}
	putPresent(invoke, "method", firstString(fields["method"]))
	putPresent(invoke, "path", firstString(fields["path"]))
	putMap(invoke, "stream", normalizeCLIStream(fields["stream"]))
	putList(invoke, "topics", anyList(sealTo(fields["topics"])))

	if len(invoke) == 0 {
		return nil
	}
	return invoke
}

func normalizeCLIStream(value any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	stream := map[string]any{}
	if list, ok := fields["operations"].([]any); ok {
		seen := map[string]bool{}
		operations := []any{}
		for _, item := range list {
			text, ok := item.(string)
			if !ok {
				continue
			}
			text = strings.ToLower(text)
			if !seen[text] {
				seen[text] = true
				operations = append(operations, text)
			}
		}
		stream["operations"] = operations
	}
	if tty, ok := fields["tty"].(bool); ok {
		stream["tty"] = tty
	}
	putPresent(stream, "encoding", firstString(fields["encoding"]))

	if len(stream) == 0 {
		return nil
	}
	return stream
}

// normalizeOutput reads the filters that the CLI runs over a reply. Every
// unknown filter goes.
func normalizeOutput(value any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}

	if private, ok := fields["private_file"].(map[string]any); ok {
		return map[string]any{
			"private_file": map[string]any{
				"operation": stringOrNil(private["operation"]),
				"id":        stringOrNil(private["id"]),
				"path":      stringOrNil(private["path"]),
			},
		}
	}

	given := fields["filter"]
	if given == nil {
		given = fields["filters"]
	}

	var names []string
	switch value := given.(type) {
	case string:
		names = []string{value}
	case []any:
		for _, item := range value {
			if text, ok := item.(string); ok {
				names = append(names, text)
			}
		}
	}

	filters := []any{}
	for _, name := range names {
		if outputFilters[name] || previewFilter.MatchString(name) {
			filters = append(filters, name)
		}
	}
	if len(filters) == 0 {
		return nil
	}
	return map[string]any{"filters": filters}
}

func normalizeVersion(value any) int64 {
	if version, ok := wholeNumber(value); ok && version > 0 {
		return version
	}
	return CLIVersion
}

func truthy(value any, fallback bool) bool {
	switch value := value.(type) {
	case nil:
		return fallback
	case bool:
		return value
	case string:
		return value == "true" || value == "1"
	default:
		if number, ok := wholeNumber(value); ok {
			return number == 1
		}
		return false
	}
}
