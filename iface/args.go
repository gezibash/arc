package iface

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"fiatjaf.com/nostr"
	"fiatjaf.com/nostr/nip19"
)

// MaxInput bounds a file or the standard input that a command reads. It is
// what 256 parts of 32 KiB hold, as base64.
const MaxInput = 6 << 20

// Values are the arguments of one command, by name. A text is a string, a
// variadic text is a []string, an integer is an int64, a switch is true, and
// an absent argument has no entry.
type Values map[string]any

// Flags are the flags of every command.
type Flags struct {
	JSON   bool
	Later  bool
	Help   bool
	DryRun bool
}

// Resolver turns what a citizen typed into a public key: 64 hex characters,
// an npub, an nprofile, a NIP-05 name, a petname, or an installed name.
type Resolver interface {
	ResolveKey(ctx context.Context, text string) (nostr.PubKey, error)
}

// Lister finds a list of keys that the citizen saved for the running
// command. A key argument that names a list stands for each of its members.
type Lister interface {
	List(name string) []nostr.PubKey
}

// members is the value of a key argument that named a list, as hex keys.
type members []string

// bind reads the words that follow a command's path.
func bind(ctx context.Context, c *Command, words []string, r Resolver, stdin io.Reader) (Values, Flags, error) {
	var flags Flags
	raw := map[string][]string{}
	var positionals []string

	byName := map[string]Arg{}
	variadicAt := -1
	n := 0
	for _, a := range c.Args {
		byName[a.Name] = a
		if a.Kind == "positional" && a.Type != "stdin" {
			if a.Variadic {
				variadicAt = n
			}
			n++
		}
	}

	for i := 0; i < len(words); i++ {
		word := words[i]
		if word == "--" {
			positionals = append(positionals, words[i+1:]...)
			break
		}
		// Every word after the first word of a variadic argument is its own,
		// so a command line such as sh -c '...' passes whole.
		if variadicAt >= 0 && len(positionals) > variadicAt {
			positionals = append(positionals, words[i:]...)
			break
		}
		if !strings.HasPrefix(word, "-") || word == "-" {
			positionals = append(positionals, word)
			continue
		}
		name, value, hasValue := strings.Cut(strings.TrimLeft(word, "-"), "=")
		switch name {
		case "json":
			flags.JSON = true
			continue
		case "later":
			flags.Later = true
			continue
		case "dry-run":
			flags.DryRun = true
			continue
		case "h", "help":
			flags.Help = true
			continue
		}
		a, ok := byName[name]
		if !ok || a.Kind == "positional" {
			return nil, flags, fmt.Errorf("unknown flag --%s", name)
		}
		if a.Kind == "switch" {
			if hasValue {
				return nil, flags, fmt.Errorf("--%s takes no value", name)
			}
			raw[name] = []string{"true"}
			continue
		}
		if !hasValue {
			if i+1 >= len(words) {
				return nil, flags, fmt.Errorf("--%s needs a value", name)
			}
			i++
			value = words[i]
		}
		raw[name] = []string{value}
	}
	if flags.Help {
		return nil, flags, nil
	}

	for _, a := range c.Args {
		if a.Kind != "positional" || a.Type == "stdin" {
			continue
		}
		if len(positionals) == 0 {
			break
		}
		if a.Variadic {
			raw[a.Name] = positionals
			positionals = nil
			break
		}
		raw[a.Name] = positionals[:1]
		positionals = positionals[1:]
	}
	if len(positionals) > 0 {
		return nil, flags, fmt.Errorf("too many arguments: %s", strings.Join(positionals, " "))
	}

	values := Values{}
	for _, a := range c.Args {
		if a.Type == "stdin" {
			body, err := io.ReadAll(io.LimitReader(stdin, MaxInput+1))
			if err != nil {
				return nil, flags, err
			}
			if len(body) > MaxInput {
				return nil, flags, fmt.Errorf("the standard input is over %d bytes", MaxInput)
			}
			values[a.Name] = string(body)
			continue
		}
		words, given := raw[a.Name]
		if !given && a.Default != "" {
			words, given = []string{a.Default}, true
		}
		if !given {
			if a.Required {
				return nil, flags, fmt.Errorf("missing %s", usageOf(a))
			}
			continue
		}
		if a.Kind == "switch" {
			values[a.Name] = true
			continue
		}
		if a.Variadic {
			values[a.Name] = words
			continue
		}
		value, err := convert(ctx, a, words[0], r)
		if err != nil {
			return nil, flags, fmt.Errorf("%s: %w", a.Name, err)
		}
		values[a.Name] = value
	}
	return values, flags, nil
}

var linesPattern = regexp.MustCompile(`^([1-9][0-9]*)?:([1-9][0-9]*)?$`)

// convert reads one argument as its type.
func convert(ctx context.Context, a Arg, text string, r Resolver) (any, error) {
	switch a.Type {
	case "text":
		return text, nil
	case "integer":
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a whole number", text)
		}
		return n, nil
	case "key":
		if l, ok := r.(Lister); ok {
			if list := l.List(text); len(list) > 0 {
				out := make(members, len(list))
				for i, pk := range list {
					out[i] = pk.Hex()
				}
				return out, nil
			}
		}
		pk, err := r.ResolveKey(ctx, text)
		if err != nil {
			return nil, err
		}
		return pk.Hex(), nil
	case "event":
		return eventRef(text)
	case "address":
		if !regexp.MustCompile(a.Pattern).MatchString(text) {
			return nil, fmt.Errorf("%q does not match %s", text, a.Pattern)
		}
		return text, nil
	case "lines":
		m := linesPattern.FindStringSubmatch(text)
		if m == nil || (m[1] == "" && m[2] == "") {
			return nil, fmt.Errorf("%q is not a range of lines: use a:b, a: or :b", text)
		}
		if m[1] != "" && m[2] != "" {
			from, _ := strconv.Atoi(m[1])
			to, _ := strconv.Atoi(m[2])
			if from > to {
				return nil, fmt.Errorf("the range %q ends before it starts", text)
			}
		}
		return text, nil
	case "path":
		if _, err := os.Lstat(text); err == nil {
			return nil, fmt.Errorf("%s exists; arc never replaces a file", text)
		}
		return text, nil
	case "file":
		return readFile(text)
	}
	return nil, fmt.Errorf("the type %q does not exist", a.Type)
}

// eventRef reads 64 hex characters, a note, an nevent or an naddr. An event
// becomes its ID, and an naddr becomes its coordinate: kind:author:d.
func eventRef(text string) (string, error) {
	if id, err := nostr.IDFromHex(text); err == nil {
		return id.Hex(), nil
	}
	prefix, value, err := nip19.Decode(text)
	if err != nil {
		return "", fmt.Errorf("%q is not an event: use hex, note, nevent or naddr", text)
	}
	switch prefix {
	case "note", "nevent":
		return value.(nostr.EventPointer).ID.Hex(), nil
	case "naddr":
		p := value.(nostr.EntityPointer)
		return fmt.Sprintf("%d:%s:%s", p.Kind, p.PublicKey.Hex(), p.Identifier), nil
	}
	return "", fmt.Errorf("%q is an %s, not an event", text, prefix)
}

// fileValue is a file that the citizen named. Templates read its name, type,
// size and sha256; the file itself renders as base64.
type fileValue struct {
	Name   string
	Type   string
	Size   int64
	SHA256 string
	Bytes  []byte
}

func (f fileValue) field(name string) any {
	switch name {
	case "name":
		return f.Name
	case "type":
		return f.Type
	case "size":
		return f.Size
	case "sha256":
		return f.SHA256
	}
	return nil
}

func (f fileValue) String() string { return base64.StdEncoding.EncodeToString(f.Bytes) }

func (f fileValue) MarshalJSON() ([]byte, error) { return json.Marshal(f.String()) }

func readFile(path string) (fileValue, error) {
	file, err := os.Open(path)
	if err != nil {
		return fileValue{}, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, MaxInput+1))
	if err != nil {
		return fileValue{}, err
	}
	if len(body) > MaxInput {
		return fileValue{}, fmt.Errorf("%s is over %d bytes", path, MaxInput)
	}
	kind := mime.TypeByExtension(filepath.Ext(path))
	if kind == "" {
		kind = http.DetectContentType(body)
	}
	sum := sha256.Sum256(body)
	return fileValue{
		Name: filepath.Base(path), Type: kind, Size: int64(len(body)),
		SHA256: hex.EncodeToString(sum[:]), Bytes: body,
	}, nil
}

// usageOf writes one argument as the help shows it.
func usageOf(a Arg) string {
	switch {
	case a.Kind == "switch":
		return "--" + a.Name
	case a.Kind == "option":
		return "--" + a.Name + " <" + a.Type + ">"
	case a.Type == "stdin":
		return "<standard input>"
	case a.Variadic:
		return "<" + a.Name + "...>"
	}
	return "<" + a.Name + ">"
}
