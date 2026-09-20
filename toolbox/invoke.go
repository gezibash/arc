package toolbox

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/gezibash/arc/identity"
	"github.com/gezibash/arc/sealedbox"
)

// A command line of an installed capability becomes one request.
//
// The capability names its commands, the arguments of each one, and how the
// body is built: one argument, a template, JSON of every argument, or the
// standard input.

// MaxCLIVersion is the newest interface that this build renders.
const MaxCLIVersion = 4

var placeholder = regexp.MustCompile(`\{\{([a-zA-Z0-9_-]+)(?:\|([a-zA-Z0-9_]+)(?::([a-zA-Z0-9_-]+))?)?\}\}`)

// The filters that a template may use.
var filters = map[string]bool{"json": true, "shell": true, "pubkey": true, "seal": true}

// ErrInvalidArguments reports a command line that the capability does not
// take. Its message names what is missing.
type ErrInvalidArguments struct{ Message string }

func (e *ErrInvalidArguments) Error() string { return e.Message }

func invalid(format string, args ...any) error {
	return &ErrInvalidArguments{Message: fmt.Sprintf(format, args...)}
}

// Context is what the filters of a template need beyond the arguments.
type Context struct {
	// Identity is the caller, which seals a body to itself.
	Identity *identity.Identity
	// Resolve turns a name or the start of a key into the public keys that
	// answer to it.
	Resolve func(query string) ([][]byte, error)
}

// Invocation is one request, ready to send.
type Invocation struct {
	// Body is what the provider receives.
	Body string
	// Meta names the method and the path of the request.
	Meta map[string]any
	// Command is the command of the capability that matched.
	Command map[string]any
	// Values are the arguments as the caller gave them.
	Values map[string]any
}

// Build reads one command line against an installed capability.
func Build(install map[string]any, argv []string, context Context) (*Invocation, error) {
	fields, _ := install["capability"].(map[string]any)
	invocation, _ := fields["invocation"].(map[string]any)
	namespace := text(install["command"])

	if cli := CLI(fields); cli != nil {
		if version, ok := wholeNumber(cli["version"]); ok && version > MaxCLIVersion {
			return nil, invalid(
				"%s needs command line interface %d, and this arc renders %d. Update arc.",
				namespace, version, MaxCLIVersion)
		}
	}

	commands := Commands(fields)
	if len(commands) == 0 {
		// A capability without a command line takes the arguments as they
		// stand.
		return &Invocation{Body: strings.Join(argv, " "), Meta: metaOf(invocation, nil)}, nil
	}

	command, rest, err := resolveCommand(fields, argv)
	if err != nil {
		return nil, err
	}

	args := argsOf(command)
	values, err := parseArgs(args, rest)
	if err != nil {
		return nil, err
	}

	body, err := renderInput(command, args, values, context)
	if err != nil {
		return nil, err
	}

	return &Invocation{
		Body: body, Meta: metaOf(invocation, command), Command: command, Values: values,
	}, nil
}

// metaOf names the method and the path of a request. A command may name its
// own.
func metaOf(invocation, command map[string]any) map[string]any {
	method := "RAW"
	path := "/"

	if invocation != nil {
		if given := text(invocation["method"]); given != "" {
			method = given
		}
		if given := text(invocation["path"]); given != "" {
			path = given
		}
	}

	if command != nil {
		if invoke, ok := command["invoke"].(map[string]any); ok {
			if given := text(invoke["method"]); given != "" {
				method = given
			}
			if given := text(invoke["path"]); given != "" {
				path = given
			}
		}
	}
	return map[string]any{"method": method, "path": path}
}

// resolveCommand finds the command that the front of the line names.
func resolveCommand(fields map[string]any, argv []string) (map[string]any, []string, error) {
	commands := Commands(fields)
	root := RootCommand(fields)

	var best map[string]any
	var bestPath []string

	for _, command := range commands {
		path := pathOf(command)
		if len(path) == 0 || len(argv) < len(path) {
			continue
		}

		matches := true
		for index, segment := range path {
			if argv[index] != segment {
				matches = false
				break
			}
		}

		if matches && len(path) > len(bestPath) {
			best, bestPath = command, path
		}
	}

	switch {
	case best != nil:
		return best, argv[len(bestPath):], nil
	case root != nil:
		return root, argv, nil
	case len(argv) == 0:
		return nil, nil, invalid("this command needs a subcommand")
	default:
		return nil, nil, invalid("unknown subcommand %s", argv[0])
	}
}

// parseArgs reads the options and the positionals of one command.
func parseArgs(args []map[string]any, argv []string) (map[string]any, error) {
	options := map[string]map[string]any{}
	var positionals []map[string]any

	for _, arg := range args {
		if arg["kind"] == "option" {
			options[flagOf(arg, text(arg["name"]))] = arg
			continue
		}
		positionals = append(positionals, arg)
	}

	values := map[string]any{}
	var tokens []string

	for index := 0; index < len(argv); index++ {
		token := argv[index]

		if !strings.HasPrefix(token, "--") {
			tokens = append(tokens, token)
			continue
		}

		spec, known := options[token]
		if !known {
			return nil, invalid("unknown option %s", token)
		}

		if spec["type"] == "boolean" {
			values[text(spec["name"])] = true
			continue
		}
		if index+1 >= len(argv) {
			return nil, invalid("the option %s needs a value", token)
		}

		index++
		values[text(spec["name"])] = argv[index]
	}

	return assignPositionals(positionals, tokens, values)
}

// assignPositionals gives each positional its token. The last positional may
// take the rest.
func assignPositionals(specs []map[string]any, tokens []string, values map[string]any) (map[string]any, error) {
	for _, spec := range specs {
		name := text(spec["name"])
		required, _ := spec["required"].(bool)
		variadic, _ := spec["variadic"].(bool)

		switch {
		case variadic:
			if len(tokens) == 0 {
				if required {
					return nil, invalid("this command needs %s", name)
				}
				return values, nil
			}

			list := make([]any, 0, len(tokens))
			for _, token := range tokens {
				list = append(list, token)
			}
			values[name] = list
			return values, nil

		case len(tokens) == 0 && required:
			return nil, invalid("this command needs %s", name)

		case len(tokens) == 0:

		default:
			values[name] = tokens[0]
			tokens = tokens[1:]
		}
	}

	if len(tokens) > 0 {
		return nil, invalid("this command takes no argument %q", tokens[0])
	}
	return values, nil
}

// renderInput builds the body of the request.
func renderInput(command map[string]any, args []map[string]any, values map[string]any, context Context) (string, error) {
	input, _ := command["input"].(map[string]any)

	if input == nil {
		// One argument and no input named: the argument is the body.
		if len(args) == 1 {
			return renderValue(values[text(args[0]["name"])], " "), nil
		}
		return "", nil
	}

	switch input["source"] {
	case "arg":
		name := text(input["name"])
		value, held := values[name]
		if !held {
			return "", invalid("this command needs %s", name)
		}

		join := " "
		if given := text(input["join_with"]); given != "" {
			join = given
		}
		return renderValue(value, join), nil

	case "template":
		return renderTemplate(text(input["template"]), values, context)

	case "json":
		return encodeJSON(values), nil

	case "stdin":
		// The caller fills this body with RenderStdin, which reads the
		// standard input, an argument, or a file.
		return "", nil

	case "agora", "sealed_file", "private_file":
		return "", invalid("this command needs a part of arc that is not built yet")

	default:
		return "", invalid("this command names an input that arc does not know")
	}
}

// RenderStdin builds the body of a command that reads the standard input.
//
// The command may name an argument that carries the body instead, or a file
// to read. The body may then be sealed to one or more readers, and a header
// line may stand above it.
func RenderStdin(command map[string]any, values map[string]any, body []byte, context Context) (string, error) {
	input, _ := command["input"].(map[string]any)
	if input == nil || input["source"] != "stdin" {
		return "", invalid("this command does not read the standard input")
	}

	bodyText := string(body)

	// An argument of the command line stands in for the standard input.
	if name := text(input["body"]); name != "" {
		if given, held := values[name]; held {
			bodyText = renderValue(given, " ")
		}
	}

	// A file stands in for both.
	if name := text(input["file"]); name != "" {
		if path := renderValue(values[name], ""); path != "" {
			data, err := readFile(path)
			if err != nil {
				return "", invalid("arc cannot read %s: %v", path, err)
			}
			bodyText = string(data)
		}
	}

	// A sealed body travels as one token for each reader.
	if targets, ok := input["seal_to"].([]any); ok && len(targets) > 0 {
		var tokens []string
		for _, item := range targets {
			sealed, err := sealTo(bodyText, text(item), values, context)
			if err != nil {
				return "", err
			}
			tokens = append(tokens, sealed...)
		}
		bodyText = strings.Join(tokens, "\n")
	}

	if template := text(input["template"]); template != "" {
		header, err := renderTemplate(template, values, context)
		if err != nil {
			return "", err
		}

		join := "\n"
		if given := text(input["join_with"]); given != "" {
			join = given
		}
		return header + join + bodyText, nil
	}
	return bodyText, nil
}

// renderTemplate fills one {{name}} template from the arguments.
func renderTemplate(template string, values map[string]any, context Context) (string, error) {
	var failure error

	out := placeholder.ReplaceAllStringFunc(template, func(raw string) string {
		if failure != nil {
			return ""
		}

		groups := placeholder.FindStringSubmatch(raw)
		name, filter, argument := groups[1], groups[2], groups[3]

		if filter != "" && !filters[filter] {
			failure = invalid("the template names a filter that arc does not know: %s", filter)
			return ""
		}

		rendered, err := renderPlaceholder(filter, argument, values[name], values, context)
		if err != nil {
			failure = err
			return ""
		}
		return rendered
	})

	if failure != nil {
		return "", failure
	}
	return strings.TrimSpace(out), nil
}

func renderPlaceholder(filter, argument string, value any, values map[string]any, context Context) (string, error) {
	switch filter {
	case "":
		return renderValue(value, " "), nil

	case "json":
		return encodeJSON(value), nil

	case "shell":
		return shellQuote(renderValue(value, " ")), nil

	case "pubkey":
		keys, err := resolvePeers(renderValue(value, " "), context)
		if err != nil {
			return "", err
		}

		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, hex.EncodeToString(key))
		}
		return strings.Join(parts, ","), nil

	case "seal":
		if argument == "" {
			return "", invalid("seal needs a target, as in {{body|seal:to}}")
		}

		tokens, err := sealTo(renderValue(value, " "), argument, values, context)
		if err != nil {
			return "", err
		}
		return strings.Join(tokens, ","), nil

	default:
		return "", invalid("the template names a filter that arc does not know: %s", filter)
	}
}

// sealTo seals a body to every peer that one argument names, or to the
// caller when the target is "me".
func sealTo(body, target string, values map[string]any, context Context) ([]string, error) {
	if target == "me" {
		if context.Identity == nil {
			return nil, invalid("this command needs an identity to seal to")
		}

		sealed, err := sealedbox.SealTo(context.Identity.PublicKey, []byte(body))
		if err != nil {
			return nil, err
		}
		return []string{encodeSealed(sealed)}, nil
	}

	keys, err := resolvePeers(renderValue(values[target], " "), context)
	if err != nil {
		return nil, err
	}

	tokens := make([]string, 0, len(keys))
	for _, key := range keys {
		// The reader is named by its Ed25519 key, so a sender seals to any
		// key that it knows.
		sealed, err := sealedbox.SealTo(key, []byte(body))
		if err != nil {
			return nil, err
		}
		tokens = append(tokens, encodeSealed(sealed))
	}
	return tokens, nil
}

// resolvePeers reads a set of peers: full keys, or names that the relay
// answers.
func resolvePeers(query string, context Context) ([][]byte, error) {
	var keys [][]byte

	for _, part := range strings.Split(query, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}

		if raw, err := hex.DecodeString(part); err == nil && len(raw) == 32 {
			keys = append(keys, raw)
			continue
		}

		if context.Resolve == nil {
			return nil, invalid("arc cannot look up %s here", part)
		}

		found, err := context.Resolve(part)
		if err != nil {
			return nil, err
		}
		if len(found) == 0 {
			return nil, invalid("no citizen answers to %s", part)
		}
		keys = append(keys, found...)
	}

	if len(keys) == 0 {
		return nil, invalid("this command names no citizen")
	}
	return keys, nil
}

// readFile reads the file that an argument names.
func readFile(path string) ([]byte, error) {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	}
	return os.ReadFile(path)
}

func encodeSealed(sealed []byte) string {
	return "sealed-v1:" + base64Std(sealed)
}

func renderValue(value any, join string) string {
	switch value := value.(type) {
	case nil:
		return ""
	case string:
		return value
	case bool:
		if value {
			return "true"
		}
		return "false"
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			parts = append(parts, renderValue(item, join))
		}
		return strings.Join(parts, join)
	default:
		return fmt.Sprint(value)
	}
}

func encodeJSON(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "null"
	}
	return string(encoded)
}

// shellQuote wraps a value in single quotes, so a shell takes it whole.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func wholeNumber(value any) (int, bool) {
	switch value := value.(type) {
	case json.Number:
		number, err := value.Int64()
		return int(number), err == nil
	case float64:
		return int(value), value == float64(int(value))
	case int:
		return value, true
	case int64:
		return int(value), true
	default:
		return 0, false
	}
}
