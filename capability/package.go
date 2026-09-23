// Package capability holds the capability package: the document that a
// provider writes, and the signed contract that a caller installs.
//
// A provider writes the document in JSON or TOML. ARC normalizes it, signs it
// with the identity of the serving citizen, and serves the signed result.
//
// The signature covers the canonical JSON of the package and its provider.
// The hash is SHA-256 of the same bytes.
package capability

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gezibash/arc/internal/canonical"
	"github.com/pelletier/go-toml/v2"
)

// PackageVersion is the version of the package document.
const PackageVersion = 1

// The defaults of a release that names none.
const (
	DefaultReleaseVersion = "0.1.0"
	DefaultChannel        = "stable"
)

// Errors of this package.
var (
	ErrInvalid         = errors.New("capability: the package is not valid")
	ErrUnsupportedFile = errors.New("capability: the file is not JSON or TOML")
	ErrSignerMismatch  = errors.New("capability: the signer is not the provider")
	ErrHashMismatch    = errors.New("capability: the hash does not match")
	ErrBadSignature    = errors.New("capability: the signature does not verify")
	ErrNotFound        = errors.New("capability: no capability of that name")
)

// LoadFile reads a capability document from a JSON or a TOML file, and
// returns the normalized package.
func LoadFile(path string) (map[string]any, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var document map[string]any

	switch strings.ToLower(filepath.Ext(path)) {
	case ".json":
		value, err := canonical.Decode(body)
		if err != nil {
			return nil, fmt.Errorf("capability: %s is not valid JSON", filepath.Base(path))
		}
		document, _ = value.(map[string]any)
	case ".toml":
		if err := toml.Unmarshal(body, &document); err != nil {
			return nil, fmt.Errorf("capability: %s is not valid TOML", filepath.Base(path))
		}
	default:
		return nil, ErrUnsupportedFile
	}

	return normalizeDocument(document)
}

// NormalizePackage returns the package in its one shape. Two documents that
// say the same thing normalize to the same bytes, and therefore to the same
// hash.
func NormalizePackage(document map[string]any) map[string]any {
	if document == nil {
		return map[string]any{}
	}

	fields, _ := document["capability"].(map[string]any)

	out := map[string]any{
		"package_version": PackageVersion,
		"capability":      NormalizeCapability(fields),
		"release":         normalizeRelease(document["release"]),
	}
	putPresent(out, "published_at", firstString(document["published_at"]))
	return out
}

// NormalizeCapability returns one capability in its one shape.
func NormalizeCapability(fields map[string]any) map[string]any {
	if fields == nil {
		fields = map[string]any{}
	}

	id := "primary"
	if given, ok := fields["id"].(string); ok && given != "" {
		id = given
	}

	out := map[string]any{
		"id":         id,
		"kind":       orNil(fields["kind"]),
		"scheme":     orNil(fields["scheme"]),
		"title":      orNil(fields["title"]),
		"summary":    orNil(fields["summary"]),
		"invocation": normalizeInvocation(fields["invocation"]),
		"examples":   anyList(stringList(fields["examples"])),
	}
	putMap(out, "config", normalizeConfig(fields["config"]))
	return out
}

// Valid says whether a package holds everything that a caller needs.
func Valid(pkg map[string]any) bool {
	fields, ok := pkg["capability"].(map[string]any)
	if !ok {
		return false
	}
	release, ok := pkg["release"].(map[string]any)
	if !ok {
		return false
	}
	invocation, ok := fields["invocation"].(map[string]any)
	if !ok {
		return false
	}

	version, _ := wholeNumber(pkg["package_version"])
	if version != PackageVersion {
		return false
	}

	for _, value := range []any{
		fields["id"], fields["kind"], fields["scheme"], fields["title"], fields["summary"],
		invocation["method"], invocation["path"],
		release["version"], release["channel"],
	} {
		if firstString(value) == "" {
			return false
		}
	}
	return true
}

func normalizeDocument(document map[string]any) (map[string]any, error) {
	fields, ok := document["capability"].(map[string]any)
	if !ok {
		return nil, ErrInvalid
	}

	// A document may hold the examples beside the capability. They belong to
	// the capability.
	capability := make(map[string]any, len(fields)+1)
	for key, value := range fields {
		capability[key] = value
	}
	if _, held := capability["examples"]; !held && document["examples"] != nil {
		capability["examples"] = document["examples"]
	}

	release := document["release"]
	published := firstString(document["published_at"])
	if published == "" {
		if fields, ok := release.(map[string]any); ok {
			published = firstString(fields["published_at"])
		}
	}

	pkg := map[string]any{
		"package_version": PackageVersion,
		"capability":      NormalizeCapability(capability),
		"release":         normalizeRelease(release),
	}
	putPresent(pkg, "published_at", published)

	if !Valid(pkg) {
		return nil, ErrInvalid
	}
	return pkg, nil
}

func normalizeRelease(value any) map[string]any {
	fields, _ := value.(map[string]any)

	version := DefaultReleaseVersion
	channel := DefaultChannel
	if fields != nil {
		if given := firstString(fields["version"]); given != "" {
			version = given
		}
		if given := firstString(fields["channel"]); given != "" {
			channel = given
		}
	}
	return map[string]any{"version": version, "channel": channel}
}

func normalizeInvocation(value any) map[string]any {
	fields, _ := value.(map[string]any)
	if fields == nil {
		fields = map[string]any{}
	}

	method := "RAW"
	if given, ok := fields["method"].(string); ok {
		method = given
	}
	path := "/"
	if given, ok := fields["path"].(string); ok {
		path = given
	}

	return map[string]any{
		"method":        method,
		"path":          path,
		"request_body":  bodyShape(fields["request_body"], "Plain text request body"),
		"response_body": bodyShape(fields["response_body"], "Plain text response body"),
	}
}

func bodyShape(value any, description string) any {
	if fields, ok := value.(map[string]any); ok {
		return fields
	}
	return map[string]any{"type": "text", "description": description}
}

func normalizeConfig(value any) map[string]any {
	fields, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}

func decodeHex(value any) ([]byte, error) {
	text, ok := value.(string)
	if !ok {
		return nil, ErrInvalid
	}
	out, err := hex.DecodeString(strings.ToLower(text))
	if err != nil {
		return nil, ErrInvalid
	}
	return out, nil
}

// wholeNumber reads a number that came from JSON, from TOML, or from Go.
func wholeNumber(value any) (int64, bool) {
	switch value := value.(type) {
	case json.Number:
		out, err := value.Int64()
		return out, err == nil
	case int:
		return int64(value), true
	case int64:
		return value, true
	case float64:
		return int64(value), value == float64(int64(value))
	default:
		return 0, false
	}
}

func firstString(values ...any) string {
	for _, value := range values {
		if text, ok := value.(string); ok && text != "" {
			return text
		}
	}
	return ""
}

func orNil(value any) any {
	if text, ok := value.(string); ok && text != "" {
		return text
	}
	return nil
}

func stringList(value any) []string {
	list, ok := value.([]any)
	if !ok {
		return nil
	}

	out := make([]string, 0, len(list))
	for _, item := range list {
		if text, ok := item.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func anyList(values []string) []any {
	out := make([]any, 0, len(values))
	for _, value := range values {
		out = append(out, value)
	}
	return out
}

func putPresent(fields map[string]any, key string, value string) {
	if value != "" {
		fields[key] = value
	}
}

func putMap(fields map[string]any, key string, value map[string]any) {
	if value != nil {
		fields[key] = value
	}
}
