// Package bundle holds a provider that lives in a directory.
//
// A bundle is a directory with two files:
//
//	Arcfile        how to run the program on this machine
//	manifest.json  the capability that the citizen announces
//
// The command `arc serve <directory>` reads the Arcfile and turns it into
// the address that the runtime already understands:
//
//	exec://<command>?manifest=<path>
package bundle

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// The names of the files of a bundle.
const (
	ArcfileName  = "Arcfile"
	ManifestName = "manifest.json"
	RuntimeName  = "run.sh"
)

// Errors of a bundle.
var (
	ErrNoArcfile      = errors.New("bundle: the directory holds no Arcfile")
	ErrVersion        = errors.New("bundle: the Arcfile names another version")
	ErrRuntime        = errors.New("bundle: the Arcfile names no exec runtime")
	ErrCommand        = errors.New("bundle: the runtime names no command")
	ErrManifest       = errors.New("bundle: the Arcfile names no manifest")
	ErrManifestAbsent = errors.New("bundle: the manifest file is missing")
)

// Bundle is one provider directory, read and resolved.
type Bundle struct {
	Root     string
	Arcfile  string
	Command  string
	Args     []string
	Cwd      string
	Manifest string
}

// document is the Arcfile itself.
type document struct {
	Version int `toml:"version"`
	Runtime struct {
		Type    string   `toml:"type"`
		Command string   `toml:"command"`
		Args    []string `toml:"args"`
		Cwd     string   `toml:"cwd"`
	} `toml:"runtime"`
	Manifest struct {
		Path string `toml:"path"`
	} `toml:"manifest"`
}

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

	var held document
	if err := toml.Unmarshal(body, &held); err != nil {
		return nil, fmt.Errorf("bundle: %w", err)
	}

	if held.Version != 1 {
		return nil, ErrVersion
	}
	if held.Runtime.Type != "exec" {
		return nil, ErrRuntime
	}
	if strings.TrimSpace(held.Runtime.Command) == "" {
		return nil, ErrCommand
	}
	if strings.TrimSpace(held.Manifest.Path) == "" {
		return nil, ErrManifest
	}

	root := filepath.Dir(arcfile)
	cwd := root
	if held.Runtime.Cwd != "" {
		cwd = resolve(root, held.Runtime.Cwd)
	}

	manifest := resolve(root, held.Manifest.Path)
	if info, err := os.Stat(manifest); err != nil || info.IsDir() {
		return nil, ErrManifestAbsent
	}

	var args []string
	for _, arg := range held.Runtime.Args {
		if arg != "" {
			args = append(args, arg)
		}
	}

	return &Bundle{
		Root: root, Arcfile: arcfile,
		Command: resolve(cwd, held.Runtime.Command),
		Args:    args, Cwd: cwd, Manifest: manifest,
	}, nil
}

// ServeURI writes the address that the runtime reads. It escapes the path of
// the command, so a #, a % or a ? in a directory name stays in the path. The
// args parameter is a JSON list, so an argument can contain a space.
func (b *Bundle) ServeURI() string {
	query := url.Values{"manifest": {b.Manifest}}
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

	for _, one := range []string{files.Arcfile, files.Manifest, files.Runtime} {
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

// namespace turns the name of the directory into a scheme.
func namespace(name string) string {
	held := manyDashes.ReplaceAllString(notName.ReplaceAllString(strings.ToLower(name), "-"), "-")
	held = strings.Trim(held, "-")
	if held == "" {
		return "app"
	}
	return held
}

// title turns the name of the directory into a title.
func title(name string) string {
	fields := strings.Fields(strings.NewReplacer("-", " ", "_", " ").Replace(name))

	for index, field := range fields {
		fields[index] = strings.ToUpper(field[:1]) + field[1:]
	}
	if len(fields) == 0 {
		return "Arc App"
	}
	return strings.Join(fields, " ")
}
