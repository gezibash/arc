package capability

import (
	"strings"

	"github.com/gezibash/arc/identity"
)

// The manifest that a serving citizen answers:
//
//	GET /info                        a short summary of every capability
//	GET /info/capabilities/<id>      the detail of one capability
const (
	ManifestVersion = 2
	SummaryPath     = "/info"
	DetailPrefix    = "/info/capabilities/"
)

// Ask names what a caller asked for.
type Ask int

// The three answers to a request for a manifest.
const (
	// AskNone means that the request is not a manifest request.
	AskNone Ask = iota
	// AskSummary means that the caller asked for the summary.
	AskSummary
	// AskDetail means that the caller asked for one capability.
	AskDetail
)

// Request reads the meta of a frame, and says what the caller asked for.
func Request(meta map[string]any) (Ask, string) {
	method := strings.ToUpper(firstString(meta["method"]))
	path := firstString(meta["path"])

	if method != "GET" {
		return AskNone, ""
	}

	switch {
	case path == SummaryPath:
		return AskSummary, ""
	case strings.HasPrefix(path, DetailPrefix):
		id := strings.TrimPrefix(path, DetailPrefix)
		if id == "" || strings.Contains(id, "/") {
			return AskNone, ""
		}
		return AskDetail, id
	default:
		return AskNone, ""
	}
}

// DetailPath is where the detail of one capability lives.
func DetailPath(id string) string { return DetailPrefix + id }

// Summary returns the short manifest of a serving citizen.
func Summary(me *identity.Identity, pkg map[string]any) (map[string]any, error) {
	signed, err := Sign(me, pkg)
	if err != nil {
		return nil, err
	}

	fields, _ := signed["capability"].(map[string]any)
	if fields == nil {
		fields = map[string]any{}
	}

	return map[string]any{
		"manifest_version": ManifestVersion,
		"view":             "summary",
		"provider":         signed["provider"],
		"capability_count": 1,
		"capabilities":     []any{summaryOf(fields, signed)},
	}, nil
}

// Detail returns the signed package of one capability.
func Detail(me *identity.Identity, pkg map[string]any, id string) (map[string]any, error) {
	fields, _ := pkg["capability"].(map[string]any)
	if fields == nil || firstString(fields["id"]) != id {
		return nil, ErrNotFound
	}

	signed, err := Sign(me, pkg)
	if err != nil {
		return nil, err
	}

	capability, _ := signed["capability"].(map[string]any)
	capability["detail_path"] = DetailPath(id)
	capability["expand"] = map[string]any{"path": DetailPath(id)}

	signed["manifest_version"] = ManifestVersion
	signed["view"] = "detail"
	return signed, nil
}

// SummaryCapabilities reads the capability list out of a summary. A caller
// uses it to find the detail path of a capability.
func SummaryCapabilities(summary map[string]any) []map[string]any {
	list, _ := summary["capabilities"].([]any)

	out := make([]map[string]any, 0, len(list))
	for _, item := range list {
		if fields, ok := item.(map[string]any); ok {
			out = append(out, fields)
		}
	}
	return out
}

func summaryOf(fields, signed map[string]any) map[string]any {
	invocation, _ := fields["invocation"].(map[string]any)
	release, _ := signed["release"].(map[string]any)

	mode := "request_reply"
	if invocation != nil {
		if given := firstString(invocation["mode"]); given != "" {
			mode = given
		}
	}

	id := firstString(fields["id"])
	return map[string]any{
		"id":              fields["id"],
		"kind":            fields["kind"],
		"scheme":          fields["scheme"],
		"title":           fields["title"],
		"summary":         fields["summary"],
		"invocation_mode": mode,
		"release_version": release["version"],
		"channel":         release["channel"],
		"detail_path":     DetailPath(id),
	}
}
