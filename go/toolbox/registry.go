// Package toolbox holds the capabilities that a citizen installed, the
// signers that its owner trusts, and the way a command line becomes one
// request.
//
// An install is a local copy of a signed capability package. The record says
// which command runs it, which citizen serves it, who signed it, and the
// hash of the package that the owner accepted.
package toolbox

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/gezibash/arc/go/capability"
	"github.com/gezibash/arc/go/identity"
)

// RegistryVersion is the version of the file that holds the installs.
const RegistryVersion = 2

// Errors of the registry.
var (
	ErrNotFound       = errors.New("toolbox: no command of that name")
	ErrNotInstallable = errors.New("toolbox: the capability offers no command line")
	ErrReserved       = errors.New("toolbox: that name belongs to arc itself")
	ErrSignerConflict = errors.New("toolbox: another signer holds that command")
	ErrInvalidCommand = errors.New("toolbox: the command name is empty")
)

// reserved names belong to arc itself, and never to an install.
var reserved = map[string]bool{
	"keys": true, "join": true, "publish": true, "resolve": true, "apps": true,
	"host": true, "discover": true, "mount": true, "mcp": true, "send": true,
	"info": true, "listen": true, "serve": true, "relay": true, "install": true,
	"tool": true, "trust": true, "help": true, "call": true, "status": true,
	"whoami": true, "request": true, "version": true, "completion": true,
	"cache": true, "lists": true, "update": true,
}

var commandPattern = regexp.MustCompile(`[^a-z0-9-]+`)
var dashes = regexp.MustCompile(`-+`)

// Store holds the installs and the trust of one machine.
type Store struct {
	// Dir is the directory of ARC, for example ~/.config/arc.
	Dir string
}

// DefaultStore returns the store of this user.
func DefaultStore() (*Store, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	return &Store{Dir: filepath.Join(home, ".config", "arc")}, nil
}

func (s *Store) toolsPath(owner []byte) string {
	return filepath.Join(s.Dir, "tools", hex.EncodeToString(owner), "tools.json")
}

// document is the file of one owner.
type document struct {
	Version        int              `json:"version"`
	OwnerName      string           `json:"owner_name"`
	OwnerPublicKey string           `json:"owner_public_key"`
	Tools          []map[string]any `json:"tools"`
}

// List returns the installs of one owner, in command order.
func (s *Store) List(owner []byte) ([]map[string]any, error) {
	held, err := s.load(owner)
	if err != nil {
		return nil, err
	}
	return held.Tools, nil
}

// Get returns one install.
func (s *Store) Get(owner []byte, command string) (map[string]any, error) {
	tools, err := s.List(owner)
	if err != nil {
		return nil, err
	}

	command = NormalizeCommand(command)
	for _, tool := range tools {
		if tool["command"] == command {
			return tool, nil
		}
	}
	return nil, ErrNotFound
}

// Installed says whether a command belongs to an install of this owner.
func (s *Store) Installed(owner []byte, command string) bool {
	_, err := s.Get(owner, command)
	return err == nil
}

// Options change one install.
type Options struct {
	// Command renames the command. Without it, the name comes from the
	// capability itself.
	Command string
	// Pinned holds the install at this version.
	Pinned bool
	// TrustState says what the owner decided about the signer.
	TrustState string
}

// Install saves one verified package as a command of this owner.
func (s *Store) Install(owner []byte, signed map[string]any, opts Options) (map[string]any, error) {
	verified, err := capability.Verify(signed)
	if err != nil {
		return nil, err
	}

	command, err := CommandName(verified, opts.Command)
	if err != nil {
		return nil, err
	}

	held, err := s.load(owner)
	if err != nil {
		return nil, err
	}

	if reserved[command] {
		return nil, ErrReserved
	}

	signature, _ := verified["signature"].(map[string]any)
	signer, _ := signature["signer_public_key"].(string)

	for _, tool := range held.Tools {
		if tool["command"] != command {
			continue
		}
		if held, _ := tool["signer_public_key"].(string); held != "" && held != signer {
			return nil, ErrSignerConflict
		}
	}

	provider, _ := verified["provider"].(map[string]any)
	fields, _ := verified["capability"].(map[string]any)
	release, _ := verified["release"].(map[string]any)

	trust := opts.TrustState
	if trust == "" {
		trust = "allowed"
	}

	install := map[string]any{
		"command":                command,
		"install_id":             text(provider["name"]) + "/" + text(fields["id"]),
		"owner_name":             identity.Name(owner),
		"owner_public_key":       hex.EncodeToString(owner),
		"provider":               provider,
		"provider_public_key":    provider["public_key"],
		"capability":             fields,
		"capability_id":          fields["id"],
		"package_version":        verified["package_version"],
		"package_hash":           verified["package_hash"],
		"release_version":        release["version"],
		"channel":                release["channel"],
		"published_at":           verified["published_at"],
		"signer_public_key":      signer,
		"signature":              signature,
		"usage":                  Usage(command, fields),
		"installed_at":           time.Now().UnixMilli(),
		"pinned":                 opts.Pinned,
		"trust_state_at_install": trust,
	}

	tools := make([]map[string]any, 0, len(held.Tools)+1)
	for _, tool := range held.Tools {
		if tool["command"] != command {
			tools = append(tools, tool)
		}
	}
	tools = append(tools, install)
	sort.Slice(tools, func(left, right int) bool {
		return text(tools[left]["command"]) < text(tools[right]["command"])
	})

	held.Tools = tools
	if err := s.save(owner, held); err != nil {
		return nil, err
	}
	return install, nil
}

// Uninstall removes one command.
func (s *Store) Uninstall(owner []byte, command string) error {
	command = NormalizeCommand(command)

	held, err := s.load(owner)
	if err != nil {
		return err
	}

	tools := make([]map[string]any, 0, len(held.Tools))
	for _, tool := range held.Tools {
		if tool["command"] != command {
			tools = append(tools, tool)
		}
	}

	held.Tools = tools
	return s.save(owner, held)
}

// SetPinned holds an install at its version, or lets it move again.
func (s *Store) SetPinned(owner []byte, command string, pinned bool) (map[string]any, error) {
	command = NormalizeCommand(command)

	held, err := s.load(owner)
	if err != nil {
		return nil, err
	}

	for _, tool := range held.Tools {
		if tool["command"] == command {
			tool["pinned"] = pinned
			if err := s.save(owner, held); err != nil {
				return nil, err
			}
			return tool, nil
		}
	}
	return nil, ErrNotFound
}

// SignedPackage builds the package again out of an install, so a caller can
// check it against what the citizen serves now.
func SignedPackage(tool map[string]any) map[string]any {
	return map[string]any{
		"package_version": tool["package_version"],
		"published_at":    tool["published_at"],
		"provider":        tool["provider"],
		"capability":      tool["capability"],
		"release": map[string]any{
			"version": tool["release_version"],
			"channel": tool["channel"],
		},
		"package_hash": tool["package_hash"],
		"signature":    tool["signature"],
	}
}

// CommandName reads the name that a capability asks for, or the name that
// the owner chose.
func CommandName(verified map[string]any, requested string) (string, error) {
	fields, _ := verified["capability"].(map[string]any)
	cli := CLI(fields)

	if cli == nil {
		return "", ErrNotInstallable
	}
	if requested == "" {
		requested = text(cli["namespace"])
	}

	command := NormalizeCommand(requested)
	if command == "" {
		return "", ErrInvalidCommand
	}
	return command, nil
}

// Reserved says whether a name belongs to arc itself.
func Reserved(command string) bool { return reserved[NormalizeCommand(command)] }

// NormalizeCommand holds a command name to lower case letters, digits and
// dashes.
func NormalizeCommand(value string) string {
	value = strings.ToLower(value)
	value = commandPattern.ReplaceAllString(value, "-")
	value = dashes.ReplaceAllString(value, "-")
	return strings.Trim(value, "-")
}

// CLI returns the command line interface of a capability, or nothing.
func CLI(fields map[string]any) map[string]any {
	if fields == nil {
		return nil
	}

	interfaces, _ := fields["interfaces"].(map[string]any)
	if interfaces == nil {
		return nil
	}

	cli, _ := interfaces["cli"].(map[string]any)
	return cli
}

// Commands returns the commands of a capability.
func Commands(fields map[string]any) []map[string]any {
	cli := CLI(fields)
	if cli == nil {
		return nil
	}

	list, _ := cli["commands"].([]any)
	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if command, ok := item.(map[string]any); ok {
			out = append(out, command)
		}
	}
	return out
}

// RootCommand is the command that stands at the front, with no path.
func RootCommand(fields map[string]any) map[string]any {
	for _, command := range Commands(fields) {
		if len(pathOf(command)) == 0 {
			return command
		}
	}
	return nil
}

// Subcommands are the commands that stand under a path.
func Subcommands(fields map[string]any) []map[string]any {
	var out []map[string]any
	for _, command := range Commands(fields) {
		if len(pathOf(command)) > 0 {
			out = append(out, command)
		}
	}
	return out
}

// Usage writes the line that shows how to run a command.
func Usage(command string, fields map[string]any) string {
	commands := Commands(fields)

	switch {
	case len(commands) == 1 && len(pathOf(commands[0])) == 0:
		return "arc " + command + argumentSuffix(argsOf(commands[0]))
	case len(commands) > 0:
		return "arc " + command + " <subcommand>"
	default:
		return "arc " + command + " [input]"
	}
}

// CommandUsage writes the line of one subcommand.
func CommandUsage(namespace string, command map[string]any) string {
	out := "arc " + namespace
	if path := pathOf(command); len(path) > 0 {
		out += " " + strings.Join(path, " ")
	}

	out += argumentSuffix(argsOf(command))
	if input, ok := command["input"].(map[string]any); ok && input["source"] == "stdin" {
		out += " < body"
	}
	return out
}

// Label names one subcommand for a listing.
func Label(command map[string]any) string {
	path := pathOf(command)
	if len(path) == 0 {
		return "(root)"
	}
	return strings.Join(path, " ")
}

func argumentSuffix(args []map[string]any) string {
	if len(args) == 0 {
		return ""
	}

	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, argumentToken(arg))
	}
	return " " + strings.Join(parts, " ")
}

func argumentToken(arg map[string]any) string {
	name := text(arg["name"])
	required, _ := arg["required"].(bool)

	var token string

	switch {
	case arg["kind"] == "option" && arg["type"] == "boolean":
		token = flagOf(arg, name)
	case arg["kind"] == "option":
		token = flagOf(arg, name) + " <" + name + ">"
	default:
		if variadic, _ := arg["variadic"].(bool); variadic {
			token = "<" + name + "...>"
		} else {
			token = "<" + name + ">"
		}
	}

	if required {
		return token
	}
	return "[" + token + "]"
}

func flagOf(arg map[string]any, name string) string {
	if flag := text(arg["flag"]); flag != "" {
		return flag
	}
	return "--" + strings.ReplaceAll(name, "_", "-")
}

func (s *Store) load(owner []byte) (*document, error) {
	held := &document{
		Version:        RegistryVersion,
		OwnerName:      identity.Name(owner),
		OwnerPublicKey: hex.EncodeToString(owner),
		Tools:          []map[string]any{},
	}

	data, err := os.ReadFile(s.toolsPath(owner))
	if errors.Is(err, os.ErrNotExist) {
		return held, nil
	}
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal(data, held); err != nil {
		return nil, fmt.Errorf("toolbox: %s does not hold together", s.toolsPath(owner))
	}
	if held.Tools == nil {
		held.Tools = []map[string]any{}
	}
	return held, nil
}

func (s *Store) save(owner []byte, held *document) error {
	held.Version = RegistryVersion
	held.OwnerName = identity.Name(owner)
	held.OwnerPublicKey = hex.EncodeToString(owner)

	data, err := json.Marshal(held)
	if err != nil {
		return err
	}

	path := s.toolsPath(owner)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func pathOf(command map[string]any) []string {
	list, _ := command["path"].([]any)

	out := make([]string, 0, len(list))
	for _, item := range list {
		if segment, ok := item.(string); ok {
			out = append(out, segment)
		}
	}
	return out
}

func argsOf(command map[string]any) []map[string]any {
	list, _ := command["args"].([]any)

	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if arg, ok := item.(map[string]any); ok {
			out = append(out, arg)
		}
	}
	return out
}

func text(value any) string {
	out, _ := value.(string)
	return out
}
