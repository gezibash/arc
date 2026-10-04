// Package bundle resolves an app directory with a service program.
//
// A bundle is a directory with two files:
//
//	Arcfile        how to serve the program on this machine
//	manifest.json  the interface that the operating identity announces
//
// An Arcfile of version 2 has two tables. [serve] says how the program goes
// into ARC: its command, the protocol that it speaks, its manifest, and the
// keys that it allows. [uses] says how the program goes out of ARC: the
// installed apps that it calls.
//
// The command `arc serve <directory>` reads the Arcfile and turns it into
// the address that the runtime already understands:
//
//	exec://<command>?manifest=<path>&args=<JSON list>&cwd=<directory>
package bundle

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/pelletier/go-toml/v2"
)

// The names of the files of a bundle.
const (
	ArcfileName   = "Arcfile"
	ManifestName  = "manifest.json"
	InterfaceName = "interface.json"
	RuntimeName   = "run.sh"
)

// Errors of a bundle.
var (
	ErrNoArcfile      = errors.New("bundle: the directory holds no Arcfile")
	ErrVersion        = errors.New("bundle: the Arcfile must say version = 2")
	ErrVersion1       = errors.New("bundle: this Arcfile has version 1. Write version = 2, rename [runtime] to [serve], remove type, and move [manifest] path to manifest in [serve]")
	ErrCommand        = errors.New("bundle: [serve] names no command")
	ErrManifest       = errors.New("bundle: [serve] names no manifest")
	ErrManifestAbsent = errors.New("bundle: the manifest file is missing")
)

// Stdio is the protocol that a program speaks when the Arcfile names none.
const Stdio = "stdio"

// Bundle is one app deployment directory, read and resolved.
type Bundle struct {
	Root    string
	Arcfile string
	// Command and Args run the program. For a protocol other than stdio,
	// Command is the translator, and Args start with the program.
	Command  string
	Args     []string
	Cwd      string
	Manifest string
	Protocol string
	// Allow holds the callers that the Arcfile allows, as written. Empty
	// allows every caller.
	Allow []string
	// Uses holds the installed apps that the program calls, by local name.
	// Nil allows every installed app.
	Uses []Use
}

// Use is one entry of [uses]: a local name and the installed app it names.
type Use struct {
	Name      string
	Installed string
}

// Env is the name of the environment variable that holds the address of
// the app. The name "geo-data" gives ARC_USE_GEO_DATA.
func (u Use) Env() string {
	return "ARC_USE_" + strings.ToUpper(strings.ReplaceAll(u.Name, "-", "_"))
}

// document is the Arcfile itself.
type document struct {
	Version int `toml:"version"`
	Serve   struct {
		Command  string   `toml:"command"`
		Args     []string `toml:"args"`
		Cwd      string   `toml:"cwd"`
		Protocol string   `toml:"protocol"`
		Manifest string   `toml:"manifest"`
		Allow    []string `toml:"allow"`
	} `toml:"serve"`
	Uses map[string]string `toml:"uses"`
}

var (
	protocolPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	useNamePattern  = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
)

var scheme = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://`)

// Resolve turns what the owner asked to serve into an address. An address
// stays as it is. A directory or an Arcfile becomes an exec address.
func Resolve(target string) (string, *Bundle, error) {
	if target == "" {
		return "", nil, errors.New("bundle: name a directory or an address")
	}
	if scheme.MatchString(target) {
		return target, nil, nil
	}

	path := target
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		path = filepath.Join(target, ArcfileName)
	} else if err != nil || filepath.Base(target) != ArcfileName {
		// Not a bundle. The runtime reads the address itself.
		return target, nil, nil
	}

	held, err := Load(path)
	if err != nil {
		return "", nil, err
	}
	return held.ServeURI(), held, nil
}

// Load reads one Arcfile and resolves every path in it.
func Load(path string) (*Bundle, error) {
	arcfile, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	body, err := os.ReadFile(arcfile)
	if err != nil {
		return nil, ErrNoArcfile
	}

	var probe map[string]any
	if err := toml.Unmarshal(body, &probe); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	version, _ := probe["version"].(int64)
	switch version {
	case 2:
	case 1:
		return nil, ErrVersion1
	default:
		return nil, ErrVersion
	}

	// A field that version 2 does not define is an error, so a typing error
	// such as "protcol" does not pass unseen.
	var held document
	decoder := toml.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&held); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}
	serve := held.Serve
	if strings.TrimSpace(serve.Command) == "" {
		return nil, ErrCommand
	}
	if strings.TrimSpace(serve.Manifest) == "" {
		return nil, ErrManifest
	}

	root := filepath.Dir(arcfile)
	cwd := root
	if serve.Cwd != "" {
		cwd = resolve(root, serve.Cwd)
	}

	manifest := resolve(root, serve.Manifest)
	if info, err := os.Stat(manifest); err != nil || info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		modern := filepath.Join(root, InterfaceName)
		if info, err := os.Stat(modern); err != nil || info.IsDir() {
			return nil, ErrManifestAbsent
		}
		manifest = modern
	}

	command, err := findCommand(cwd, serve.Command)
	if err != nil {
		return nil, err
	}
	var args []string
	for _, arg := range serve.Args {
		if arg != "" {
			args = append(args, arg)
		}
	}

	protocol := serve.Protocol
	if protocol == "" {
		protocol = Stdio
	}
	if !protocolPattern.MatchString(protocol) {
		return nil, fmt.Errorf("bundle: the protocol %q must be lower-case letters, digits and hyphens", protocol)
	}
	if protocol != Stdio {
		translator, err := findTranslator(protocol)
		if err != nil {
			return nil, err
		}
		args = append([]string{command}, args...)
		command = translator
	}

	// An empty [uses] allows no call. A missing one allows every installed
	// app. The decoder gives both as nil, so the table is looked up.
	var uses []Use
	if _, listed := probe["uses"]; listed {
		uses = []Use{}
		for name, installed := range held.Uses {
			if !useNamePattern.MatchString(name) {
				return nil, fmt.Errorf("bundle: the name %q in [uses] must be lower-case letters, digits and hyphens", name)
			}
			if strings.TrimSpace(installed) == "" {
				return nil, fmt.Errorf("bundle: %q in [uses] names no installed app", name)
			}
			uses = append(uses, Use{Name: name, Installed: installed})
		}
		sort.Slice(uses, func(a, b int) bool { return uses[a].Name < uses[b].Name })
	}

	return &Bundle{
		Root: root, Arcfile: arcfile,
		Command: command, Args: args, Cwd: cwd, Manifest: manifest,
		Protocol: protocol, Allow: serve.Allow, Uses: uses,
	}, nil
}

// findCommand resolves the command as a shell does. A name with a slash is
// a path from the directory of the program. A name without one is looked
// up in PATH.
func findCommand(cwd, command string) (string, error) {
	if strings.ContainsRune(command, '/') {
		return resolve(cwd, command), nil
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("bundle: the command %q is not on PATH. Name a script with ./, or install the command", command)
	}
	return path, nil
}

// findTranslator finds the program arc-<protocol>. It looks beside the arc
// program first, where a release puts the translators, then in PATH.
func findTranslator(protocol string) (string, error) {
	name := "arc-" + protocol
	if self, err := os.Executable(); err == nil {
		beside := filepath.Join(filepath.Dir(self), name)
		if info, err := os.Stat(beside); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return beside, nil
		}
	}
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("bundle: the protocol %q needs the program %s. Put it beside arc or in PATH", protocol, name)
	}
	return path, nil
}

// ServeURI writes the address that the runtime reads. It escapes the path of
// the command, so a #, a % or a ? in a directory name stays in the path. The
// args parameter is a JSON list, so an argument can contain a space. The cwd
// parameter names the directory that the program runs in.
func (b *Bundle) ServeURI() string {
	query := url.Values{"manifest": {b.Manifest}, "cwd": {b.Cwd}}
	if len(b.Args) > 0 {
		encoded, _ := json.Marshal(b.Args)
		query.Set("args", string(encoded))
	}
	address := url.URL{Scheme: "exec", Path: b.Command, RawQuery: query.Encode()}
	return address.String()
}

// Files are the paths that Init writes.
type Files struct {
	Root     string
	Arcfile  string
	Manifest string
	Runtime  string
}

// Init writes a new bundle in a directory. It refuses to write over a file
// that is already there.
func Init(path string) (*Files, error) {
	root, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}

	name := filepath.Base(root)
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "app"
	}
	space := namespace(name)

	files := &Files{
		Root:     root,
		Arcfile:  filepath.Join(root, ArcfileName),
		Manifest: filepath.Join(root, ManifestName),
		Runtime:  filepath.Join(root, RuntimeName),
	}

	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}

	for _, one := range []string{files.Arcfile, files.Manifest, filepath.Join(root, InterfaceName), files.Runtime} {
		if _, err := os.Stat(one); err == nil {
			return nil, fmt.Errorf("bundle: %s is already there", one)
		}
	}

	if err := os.WriteFile(files.Arcfile, []byte(arcfileTemplate), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(files.Manifest, []byte(manifestTemplate(space, title(name))), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(files.Runtime, []byte(runtimeTemplate(space)), 0o755); err != nil {
		return nil, err
	}
	return files, nil
}

func resolve(root, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(root, path))
}

var notName = regexp.MustCompile(`[^a-z0-9-]+`)
var manyDashes = regexp.MustCompile(`-+`)

// namespace turns the name of the directory into a scheme. The scheme is
// also the id of the interface, so it starts with a letter and has at most
// 64 characters.
func namespace(name string) string {
	held := manyDashes.ReplaceAllString(notName.ReplaceAllString(strings.ToLower(name), "-"), "-")
	held = strings.Trim(held, "-")
	if held == "" {
		return "app"
	}
	if held[0] >= '0' && held[0] <= '9' {
		held = "app-" + held
	}
	if len(held) > 64 {
		held = strings.TrimRight(held[:64], "-")
	}
	return held
}

// title turns the name of the directory into a title. It capitalizes the
// first letter of each word. A letter can take more than one byte.
func title(name string) string {
	fields := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(name))

	for index, field := range fields {
		first, size := utf8.DecodeRuneInString(field)
		fields[index] = string(unicode.ToUpper(first)) + field[size:]
	}
	if len(fields) == 0 {
		return "Arc App"
	}
	return strings.Join(fields, " ")
}
