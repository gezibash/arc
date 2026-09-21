package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/gezibash/arc/provider"
)

// A command runs against the journal in the name of its caller.

var (
	uriPattern     = regexp.MustCompile(`(?i)^[a-z][a-z0-9+.-]*:.+`)
	pubkeyPattern  = regexp.MustCompile(`^[a-f0-9]{16,}$`)
	linesPattern   = regexp.MustCompile(`^(\d+):(\d+)$`)
	wholePattern   = regexp.MustCompile(`^-?\d+$`)
	kpiMarkPattern = regexp.MustCompile(`<!-- kpi: *([^ >]+) *-->`)
)

// request is one command of one caller.
type request struct {
	from    string
	body    string
	hasBody bool
	args    []string
	options map[string]any
}

// run reads one message and answers it.
func (s *server) run(from, message string) (string, error) {
	if from == "" {
		return "", provider.Error("forbidden no caller key")
	}

	line, body, hasBody := provider.SplitMessage(message)
	args, options := provider.ParseLine(line)

	return s.dispatch(&request{from: from, body: body, hasBody: hasBody, args: args, options: options})
}

func (s *server) dispatch(ctx *request) (string, error) {
	if len(ctx.args) == 0 {
		return help(), nil
	}

	rest := ctx.args[1:]

	switch ctx.args[0] {
	case "ls":
		where := ""
		if len(rest) == 1 {
			where = rest[0]
		}
		return s.list(ctx, where)

	case "read":
		if len(rest) != 1 {
			return "", errInvalidAddress
		}
		return s.read(ctx, rest[0])

	case "history":
		if len(rest) != 1 {
			return "", errInvalidAddress
		}
		return s.history(ctx, rest[0])

	case "write":
		if len(rest) != 1 {
			return "", errInvalidAddress
		}
		return s.write(ctx, rest[0])

	case "append":
		if len(rest) < 2 {
			return "", errInvalidAddress
		}
		return s.append(ctx, rest[0], strings.Join(rest[1:], " "))

	case "edit":
		if len(rest) != 1 {
			return "", errInvalidAddress
		}
		return s.edit(ctx, rest[0])

	case "attach":
		if len(rest) != 1 {
			return "", errInvalidAddress
		}
		return s.attach(ctx, rest[0])

	case "fetch":
		if len(rest) != 1 {
			return "", errNotFound
		}
		return s.fetch(rest[0])

	case "link":
		if len(rest) != 2 {
			return "", errInvalidAddress
		}
		return s.link(ctx, rest[0], rest[1])

	case "kpi":
		return s.kpiCommand(ctx, rest)

	case "search":
		return s.search(ctx, strings.Join(rest, " "))

	case "acl":
		return s.aclCommand(ctx, rest)

	case "help":
		return help(), nil

	default:
		return "", provider.Error("unknown_command " + ctx.args[0])
	}
}

// -- listing ----------------------------------------------------------------

func (s *server) list(ctx *request, where string) (string, error) {
	var parts []string
	for _, part := range strings.Split(where, "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}

	switch len(parts) {
	case 0:
		return lines(s.store.listProjects(ctx.from), "no projects"), nil

	case 1:
		if err := s.store.authorizeRead(parts[0], ctx.from); err != nil {
			return "", err
		}

		var rows []string
		for _, notebook := range s.store.listNotebooks(parts[0]) {
			rows = append(rows, parts[0]+"/"+notebook)
		}
		return lines(rows, "no notebooks"), nil

	case 2:
		if err := s.store.authorizeRead(parts[0], ctx.from); err != nil {
			return "", err
		}

		var rows []string
		for _, page := range s.store.listPages(parts[0], parts[1]) {
			rows = append(rows, page[0]+"\t"+page[1])
		}
		return lines(rows, "no pages"), nil

	default:
		return "", errInvalidAddress
	}
}

// -- reading ----------------------------------------------------------------

func (s *server) read(ctx *request, addr string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}
	if err := s.store.authorizeRead(parts[0], ctx.from); err != nil {
		return "", err
	}

	held, rev, err := s.store.readPage(parts)
	if err != nil {
		return "", err
	}

	text := s.injectKPIs(held, parts).render()
	if given, ok := provider.Option(ctx.options, "lines"); ok {
		text = slice(text, given)
	}
	return "rev: " + rev + "\n" + text, nil
}

func (s *server) history(ctx *request, addr string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}
	if err := s.store.authorizeRead(parts[0], ctx.from); err != nil {
		return "", err
	}

	out := s.store.history(pagePath(parts))
	if out == "" {
		return "", errNotFound
	}
	return out, nil
}

// injectKPIs replaces each marker of a page with the latest measurement.
func (s *server) injectKPIs(held *page, parts []string) *page {
	if !strings.Contains(held.Body, "<!-- kpi:") {
		return held
	}

	latest := map[string]kpi{}
	for _, one := range s.store.kpiLatest(parts[:2]) {
		latest[one.Key] = one
	}

	body := kpiMarkPattern.ReplaceAllStringFunc(held.Body, func(whole string) string {
		groups := kpiMarkPattern.FindStringSubmatch(whole)
		one, held := latest[groups[1]]
		if !held {
			return whole
		}
		return fmt.Sprintf("%s = %s (%s)", groups[1], one.value(), one.T)
	})

	return &page{Meta: held.Meta, Body: body}
}

// -- writing ----------------------------------------------------------------

func (s *server) write(ctx *request, addr string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}

	body, err := writeBody(ctx)
	if err != nil {
		return "", err
	}
	if err := s.store.authorizeWrite(parts[0], ctx.from); err != nil {
		return "", err
	}

	rev, _ := provider.Option(ctx.options, "if_rev")
	if err := s.store.checkRev(parts, rev); err != nil {
		return "", err
	}

	held, _, err := s.store.readPage(parts)
	if err != nil {
		held = &page{Meta: map[string]any{}}
	}

	if title, ok := provider.Option(ctx.options, "title"); ok {
		held.Meta["title"] = title
	}
	if given, ok := provider.Option(ctx.options, "tags"); ok {
		held.Meta["tags"] = tags(given)
	}
	held.Body = body

	written, err := s.store.writePage(parts, held, ctx.from, "write "+addr)
	if err != nil {
		return "", err
	}
	return "rev: " + written, nil
}

func (s *server) append(ctx *request, addr, text string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}
	if err := s.store.authorizeWrite(parts[0], ctx.from); err != nil {
		return "", err
	}

	held, _, err := s.store.readPage(parts)
	if err != nil {
		held = &page{Meta: map[string]any{}}
	}
	held.Body = strings.TrimRight(held.Body, "\n") + "\n\n" + text + "\n"

	written, err := s.store.writePage(parts, held, ctx.from, "append "+addr)
	if err != nil {
		return "", err
	}
	return "rev: " + written, nil
}

func (s *server) edit(ctx *request, addr string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}

	rev, hasRev := provider.Option(ctx.options, "if_rev")
	if !hasRev {
		return "", provider.Error("missing --if-rev")
	}
	find, hasFind := provider.Option(ctx.options, "find")
	if !hasFind {
		return "", provider.Error("missing --find")
	}

	if err := s.store.authorizeWrite(parts[0], ctx.from); err != nil {
		return "", err
	}
	if err := s.store.checkRev(parts, rev); err != nil {
		return "", err
	}

	held, _, err := s.store.readPage(parts)
	if err != nil {
		return "", err
	}
	if !strings.Contains(held.Body, find) {
		return "", provider.Error("not_found find string absent")
	}

	replace, _ := provider.Option(ctx.options, "replace")
	held.Body = strings.Replace(held.Body, find, replace, 1)

	written, err := s.store.writePage(parts, held, ctx.from, "edit "+addr)
	if err != nil {
		return "", err
	}
	return "rev: " + written, nil
}

// -- attachments and links --------------------------------------------------

func (s *server) attach(ctx *request, addr string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}

	name, ok := provider.Option(ctx.options, "name")
	if !ok {
		return "", provider.Error("missing --name")
	}

	encoded, ok := provider.Option(ctx.options, "base64")
	if !ok {
		return "", provider.Error("missing --base64")
	}

	data, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(encoded), ""))
	if err != nil {
		return "", provider.Error("invalid_base64")
	}
	if len(data) > MaxBlobBytes {
		return "", provider.Error(fmt.Sprintf("too_large max %d bytes", MaxBlobBytes))
	}

	if err := s.store.authorizeWrite(parts[0], ctx.from); err != nil {
		return "", err
	}

	held, _, err := s.store.readPage(parts)
	if err != nil {
		return "", err
	}

	sha, err := s.store.putBlob(data)
	if err != nil {
		return "", err
	}

	entry := map[string]any{"name": name, "sha256": sha, "bytes": len(data)}
	held.Meta["attachments"] = appendEntry(held.Meta["attachments"], entry)

	written, err := s.store.writePage(parts, held, ctx.from, "attach "+addr+" "+name)
	if err != nil {
		return "", err
	}
	return "rev: " + written + "\nsha256: " + sha, nil
}

func (s *server) fetch(sha string) (string, error) {
	data, err := s.store.getBlob(sha)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(data), nil
}

func (s *server) link(ctx *request, addr, uri string) (string, error) {
	parts, err := address(addr, 3)
	if err != nil {
		return "", err
	}
	if !uriPattern.MatchString(uri) {
		return "", provider.Error("invalid_uri")
	}
	if err := s.store.authorizeWrite(parts[0], ctx.from); err != nil {
		return "", err
	}

	held, _, err := s.store.readPage(parts)
	if err != nil {
		return "", err
	}

	entry := map[string]any{"uri": uri}
	if name, ok := provider.Option(ctx.options, "name"); ok {
		entry["name"] = name
	}
	if sha, ok := provider.Option(ctx.options, "sha256"); ok {
		entry["sha256"] = sha
	}
	// A file on the machine of the caller is named with that caller, so a
	// reader knows where it lies.
	if strings.HasPrefix(uri, "file://") {
		entry["host"] = ctx.from
	}

	held.Meta["links"] = appendEntry(held.Meta["links"], entry)

	written, err := s.store.writePage(parts, held, ctx.from, "link "+addr)
	if err != nil {
		return "", err
	}
	return "rev: " + written, nil
}

// -- the numbers ------------------------------------------------------------

func (s *server) kpiCommand(ctx *request, rest []string) (string, error) {
	if len(rest) == 0 {
		return "", provider.Error("unknown_command kpi")
	}

	switch rest[0] {
	case "set":
		if len(rest) != 4 {
			return "", errInvalidAddress
		}
		return s.kpiSet(ctx, rest[1], rest[2], rest[3])

	case "log":
		if len(rest) != 3 {
			return "", errInvalidAddress
		}
		parts, err := s.readNotebook(ctx, rest[1])
		if err != nil {
			return "", err
		}

		var rows []string
		for _, one := range s.store.kpiRead(parts) {
			if one.Key == rest[2] {
				rows = append(rows, one.format())
			}
		}
		return lines(rows, "no records"), nil

	case "latest":
		if len(rest) != 2 {
			return "", errInvalidAddress
		}
		parts, err := s.readNotebook(ctx, rest[1])
		if err != nil {
			return "", err
		}

		var rows []string
		for _, one := range s.store.kpiLatest(parts) {
			rows = append(rows, one.format())
		}
		return lines(rows, "no records"), nil

	default:
		return "", provider.Error("unknown_command kpi " + rest[0])
	}
}

func (s *server) kpiSet(ctx *request, notebook, key, value string) (string, error) {
	parts, err := address(notebook, 2)
	if err != nil {
		return "", err
	}

	number, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return "", provider.Error("invalid_number")
	}
	if err := s.store.authorizeWrite(parts[0], ctx.from); err != nil {
		return "", err
	}

	record := kpi{
		T: timestamp(), By: ctx.from, Key: key, Value: number,
		whole: wholePattern.MatchString(value),
	}
	record.Ref, _ = provider.Option(ctx.options, "ref")
	record.Note, _ = provider.Option(ctx.options, "note")

	if err := s.store.kpiAppend(parts, record, ctx.from); err != nil {
		return "", err
	}
	return record.format(), nil
}

func (s *server) readNotebook(ctx *request, notebook string) ([]string, error) {
	parts, err := address(notebook, 2)
	if err != nil {
		return nil, err
	}
	if err := s.store.authorizeRead(parts[0], ctx.from); err != nil {
		return nil, err
	}
	return parts, nil
}

// -- search and the access list ---------------------------------------------

func (s *server) search(ctx *request, query string) (string, error) {
	if query == "" {
		return "", provider.Error("missing query")
	}

	scope, _ := provider.Option(ctx.options, "notebook")
	if scope == "" {
		scope, _ = provider.Option(ctx.options, "project")
	}

	out, err := s.index.search(query, provider.Flag(ctx.options, "deep"))
	if err != nil {
		return "", err
	}

	var rows []string
	for _, line := range strings.Split(out, "\n") {
		line = projectPrefix.ReplaceAllString(line, "")
		if line == "" {
			continue
		}
		if scope != "" && strings.Contains(line, "/") && !strings.HasPrefix(line, scope) {
			continue
		}
		if scope != "" && !s.allowedLine(ctx, line) {
			continue
		}
		rows = append(rows, line)
	}
	return lines(rows, "no results"), nil
}

var projectPrefix = regexp.MustCompile(`^\./?(.*?/)?projects/`)

func (s *server) allowedLine(ctx *request, line string) bool {
	project, _, found := strings.Cut(line, "/")
	if !found {
		return true
	}
	return s.store.allowed(project, ctx.from)
}

func (s *server) aclCommand(ctx *request, rest []string) (string, error) {
	if len(rest) < 2 {
		return "", provider.Error("unknown_command acl")
	}

	project := rest[0]

	if rest[1] == "ls" {
		if err := s.store.authorizeRead(project, ctx.from); err != nil {
			return "", err
		}
		return lines(s.store.acl(project), "no acl"), nil
	}

	if (rest[1] != "add" && rest[1] != "rm") || len(rest) != 3 {
		return "", provider.Error("unknown_command acl " + rest[1])
	}

	// Only the owner changes the list, and never removes itself.
	switch {
	case !s.store.projectExists(project):
		return "", errNotFound
	case s.store.owner(project) != ctx.from:
		return "", provider.Error("forbidden owner only")
	}

	key := rest[2]
	if !pubkeyPattern.MatchString(key) {
		return "", provider.Error("invalid_pubkey")
	}

	keys := s.store.acl(project)
	if rest[1] == "add" {
		if !held(keys, key) {
			keys = append(keys, key)
		}
	} else if key != keys[0] {
		keys = without(keys, key)
	}

	message := fmt.Sprintf("acl %s %s %s", project, rest[1], cut(key, 12))
	if err := s.store.writeACL(project, keys, ctx.from, message); err != nil {
		return "", err
	}
	return strings.Join(keys, "\n"), nil
}

// -- helpers ----------------------------------------------------------------

// writeBody takes the body from --body, or from the text after the first
// line. Setting both is an error.
func writeBody(ctx *request) (string, error) {
	option, hasOption := provider.Option(ctx.options, "body")
	body := ctx.body
	hasRequestBody := ctx.hasBody && body != ""

	switch {
	case !hasOption && !hasRequestBody:
		return "", provider.Error("missing body: pass --body or send it after the first line")
	case hasOption && hasRequestBody:
		return "", provider.Error("invalid_arguments --body and request body both set")
	case hasOption:
		return trailingNewline(option), nil
	default:
		return trailingNewline(body), nil
	}
}

func trailingNewline(body string) string {
	if body == "" || strings.HasSuffix(body, "\n") {
		return body
	}
	return body + "\n"
}

func tags(given string) []any {
	var out []any
	for _, tag := range strings.Split(given, ",") {
		if tag = strings.TrimSpace(tag); tag != "" {
			out = append(out, tag)
		}
	}
	return out
}

// slice keeps the lines of a range, counted from one.
func slice(text, given string) string {
	groups := linesPattern.FindStringSubmatch(given)
	if groups == nil {
		return text
	}

	first, _ := strconv.Atoi(groups[1])
	last, _ := strconv.Atoi(groups[2])

	rows := strings.Split(text, "\n")
	if first < 1 {
		first = 1
	}
	if last > len(rows) {
		last = len(rows)
	}
	if first > last {
		return ""
	}
	return strings.Join(rows[first-1:last], "\n")
}

func appendEntry(held any, entry map[string]any) []any {
	list, _ := asList(held)
	return append(list, entry)
}

func lines(rows []string, empty string) string {
	if len(rows) == 0 {
		return empty
	}
	return strings.Join(rows, "\n")
}

func held(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
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

func help() string {
	return strings.TrimRight(`journal commands
  ls [project[/notebook]]
  read <p/n/page> [--lines a:b]
  write <p/n/page> [--title t] [--tags a,b] [--if-rev r] [--body "..."]
    the text after the first line is the page body when --body is absent
  append <p/n/page> <text>
  edit <p/n/page> --if-rev r --find s --replace t
  attach <p/n/page> --name f --base64 b
  fetch <sha256>
  link <p/n/page> <uri> [--name n] [--sha256 h]
  kpi set <p/n> <key> <value> [--ref r] [--note n]
  kpi log <p/n> <key>
  kpi latest <p/n>
  search <query> [--project p] [--notebook p/n] [--deep]
  history <p/n/page>
  acl <project> add|rm|ls [pubkey]
`, "\n")
}
