package iface

import (
	"bufio"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"fiatjaf.com/nostr"
)

// Env is what a command needs from the citizen's machine.
type Env interface {
	Resolver
	Store
	// Me is the citizen's public key.
	Me() nostr.PubKey
	// Keyed returns the first 16 bytes of HMAC-SHA256(HKDF-SHA256(secret
	// key, info), input), as unpadded base64url.
	// KeyedValue computes it from a secret key.
	Keyed(info []byte, input string) (string, error)
	// Name is the name that the citizen knows a key by.
	Name(nostr.PubKey) string
	// Call sends one request to a provider. A live call returns the reply. A
	// later call waits in the outbox, and returns with Queued set.
	Call(ctx context.Context, provider nostr.PubKey, request CallRequest, later bool) (CallResult, error)
}

// CallRequest is one request to a provider.
type CallRequest struct {
	Capability string
	Method     string
	Path       string
	Body       string
}

// CallResult is the answer to one request.
type CallResult struct {
	Queued bool
	Body   string
	Err    string
}

// Installed is a manifest that the citizen trusts, and its author.
type Installed struct {
	Manifest *Manifest
	Author   nostr.PubKey
	// Name is what the citizen types to run it.
	Name string
}

// KeyedRootD is the d tag of the draft that holds a citizen's keyed root. A
// d tag of a manifest cannot start with "arc-", so no capability replaces it.
const KeyedRootD = "arc-keyed-root"

// KeyedRootKind is the kind inside that draft: application data, NIP-78.
const KeyedRootKind = 30078

// KeyedRoot derives the root of every keyed value from a secret key. A
// machine with the key derives it; a machine that signs through NIP-46 reads
// it from the draft that a machine with the key published.
func KeyedRoot(secret [32]byte) ([]byte, error) {
	return hkdf.Key(sha256.New, secret[:], nil, "arc-keyed-root-v1", 32)
}

// KeyedValue computes a keyed value, as section 6.1 of the spec defines.
func KeyedValue(root []byte, info []byte, input string) (string, error) {
	key, err := hkdf.Key(sha256.New, root, nil, string(info), 32)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(input))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16]), nil
}

// keyedInfo is the HKDF info of one purpose of one capability.
func keyedInfo(author nostr.PubKey, id, purpose string) []byte {
	return []byte("arc-keyed-v1\x00" + author.Hex() + "\x00" + id + "\x00" + purpose)
}

// Record is one item that the pipeline works on.
type Record map[string]any

// run holds one command as it runs.
type run struct {
	ctx     context.Context
	env     Env
	in      Installed
	command *Command
	values  Values
	flags   Flags
	stdio   Stdio
	// missing is the first part that join could not find.
	missing error
	// seen is the last text that tail showed, by draft.
	seen map[string]string
}

func (r *run) keyed(purpose, input string) (string, error) {
	return r.env.Keyed(keyedInfo(r.in.Author, r.in.Manifest.ID, purpose), input)
}

// eventAuthor finds the author of an event: in its coordinate, or by
// fetching it from the group relay and the citizen's relays.
func (r *run) eventAuthor(ref string) (string, error) {
	if parts := strings.SplitN(ref, ":", 3); len(parts) == 3 {
		pk, err := nostr.PubKeyFromHex(parts[1])
		return pk.Hex(), err
	}
	id, err := nostr.IDFromHex(ref)
	if err != nil {
		return "", err
	}
	for _, relays := range [][]string{r.groupRelays(), nil} {
		if relays == nil && r.in.Manifest.Group != nil && len(r.groupRelays()) == 0 {
			continue
		}
		events, err := r.env.Fetch(r.ctx, nostr.Filter{IDs: []nostr.ID{id}}, relays)
		if err == nil && len(events) > 0 {
			return events[0].PubKey.Hex(), nil
		}
	}
	return "", fmt.Errorf("no relay holds the event %s", ref)
}

// groupRelays names the relay of the capability's group, when it has one.
func (r *run) groupRelays() []string {
	if g := r.in.Manifest.Group; g != nil {
		return []string{g.Relay}
	}
	return nil
}

func (r *run) name(key string) string {
	pk, err := nostr.PubKeyFromHex(key)
	if err != nil {
		return key
	}
	return r.env.Name(pk)
}

// scope finds the arguments and the names that every template knows.
func (r *run) scope(name string) (any, bool) {
	switch name {
	case "me":
		return r.env.Me().Hex(), true
	case "author":
		return r.in.Author.Hex(), true
	case "now":
		return time.Now().Unix(), true
	}
	value, ok := r.values[name]
	return value, ok
}

// template renders one action template as text.
func (r *run) template(text string) (string, error) {
	t, err := compile(text, nil)
	if err != nil {
		return "", err
	}
	return t.render(r.scope, r, false)
}

// Stdio are the streams of one command.
type Stdio struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Run runs one command of an installed capability. The words follow the
// capability's name.
func Run(ctx context.Context, env Env, in Installed, words []string, stdio Stdio) error {
	asksHelp := len(words) > 0 && (words[0] == "help" || words[0] == "-h" || words[0] == "--help")
	command, rest, ok := in.Manifest.Find(words)
	if ok && len(command.Path) == 0 && (asksHelp || len(words) == 0 && hasPositional(command)) {
		ok = false
	}
	if !ok {
		fmt.Fprint(stdio.Out, Help(in))
		if len(words) > 0 && !asksHelp {
			return fmt.Errorf("%s has no command %q", in.Name, strings.Join(words, " "))
		}
		return nil
	}

	values, flags, err := bind(ctx, command, rest, env, stdio.In)
	if err != nil {
		return fmt.Errorf("%w\nusage: %s", err, usage(in, command))
	}
	if flags.Help {
		fmt.Fprint(stdio.Out, commandHelp(in, command))
		return nil
	}

	// A key argument that named a list runs the command once for each
	// member.
	var listed string
	for name, value := range values {
		if _, ok := value.(members); ok {
			if listed != "" {
				return fmt.Errorf("%s and %s both name a list: one command takes one list", listed, name)
			}
			listed = name
		}
	}
	if listed == "" {
		return runOnce(ctx, env, in, command, values, flags, stdio)
	}
	if command.Action.Watch != nil {
		return fmt.Errorf("%s names a list, and a watch takes one key", listed)
	}
	list := values[listed].(members)
	failed := 0
	for _, member := range list {
		one := Values{}
		for name, value := range values {
			one[name] = value
		}
		one[listed] = member
		if err := runOnce(ctx, env, in, command, one, flags, stdio); err != nil {
			pk, _ := nostr.PubKeyFromHex(member)
			fmt.Fprintf(stdio.Err, "%s: %v\n", env.Name(pk), err)
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d members of the list failed", failed, len(list))
	}
	return nil
}

// runOnce runs a command with its values.
func runOnce(ctx context.Context, env Env, in Installed, command *Command, values Values, flags Flags, stdio Stdio) error {
	r := &run{ctx: ctx, env: env, in: in, command: command, values: values, flags: flags, stdio: stdio}
	if command.Action.Watch != nil {
		return r.watch(command.Action.Watch)
	}
	entries, err := r.act()
	if err != nil {
		return err
	}
	if entries == nil {
		return nil
	}
	if r.streamable() {
		return r.stream(entries)
	}
	if err := r.show(entries); err != nil {
		return err
	}
	return r.missing
}

func hasPositional(c *Command) bool {
	for _, a := range c.Args {
		if a.Kind == "positional" && a.Type != "stdin" {
			return true
		}
	}
	return false
}

// act runs the action. It returns no entries when there is nothing to show.
func (r *run) act() ([]*entry, error) {
	a := r.command.Action
	switch {
	case a.Call != nil:
		return r.call(a.Call)
	case a.Publish != nil:
		entries, err := r.publish(a.Publish)
		if err != nil || r.command.Output.Format == "" {
			return nil, err
		}
		return entries, nil
	case a.Delete != nil:
		return r.remove(a.Delete)
	case a.Query != nil:
		return r.query(a.Query)
	}
	return nil, errors.New("the command has no action")
}

func (r *run) call(c *Call) ([]*entry, error) {
	service := r.in.Manifest.Service
	request := CallRequest{Capability: r.in.Manifest.ID, Method: service.Method, Path: service.Path}
	var err error
	if c.Method != "" {
		if request.Method, err = r.template(c.Method); err != nil {
			return nil, err
		}
	}
	if c.Path != "" {
		if request.Path, err = r.template(c.Path); err != nil {
			return nil, err
		}
	}
	if request.Body, err = r.template(c.Body); err != nil {
		return nil, err
	}
	if request.Path == "" {
		request.Path = "/"
	}
	request.Method = strings.ToUpper(request.Method)
	if service.MaxBytes > 0 && len(request.Body) > service.MaxBytes {
		return nil, fmt.Errorf("the request is %d bytes; %s takes at most %d", len(request.Body), r.in.Name, service.MaxBytes)
	}

	later := c.Class == "later" || r.flags.Later
	result, err := r.env.Call(r.ctx, r.in.Author, request, later)
	if err != nil {
		return nil, err
	}
	if result.Queued {
		fmt.Fprintf(r.stdio.Err, "queued for %s: the reply arrives with a sync; see arc call results\n", r.env.Name(r.in.Author))
		return nil, nil
	}
	if result.Err != "" {
		return nil, fmt.Errorf("the provider refused: %s", result.Err)
	}
	return []*entry{{rec: Record{"content": result.Body}}}, nil
}

// fixed are the fields that parsed content cannot replace.
var fixed = map[string]bool{"id": true, "kind": true, "author": true, "created": true, "tags": true, "content": true, "error": true}

// open parses the content of one record into its fields.
func open(record Record, parse string) error {
	content := str(record["content"])
	switch parse {
	case "text":
		record["text"] = content
	case "json":
		decoder := json.NewDecoder(strings.NewReader(content))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			return fmt.Errorf("the reply is not JSON: %w", err)
		}
		if fields, ok := value.(map[string]any); ok {
			for key, v := range fields {
				if !fixed[key] {
					record[key] = v
				}
			}
		}
	case "frontmatter":
		fields, text := frontmatter(content)
		for key, v := range fields {
			if !fixed[key] {
				record[key] = v
			}
		}
		record["text"] = text
	}
	return nil
}

// frontmatter reads "key: value" lines between two lines of three hyphens.
func frontmatter(content string) (map[string]any, string) {
	fields := map[string]any{}
	if !strings.HasPrefix(content, "---\n") {
		return fields, content
	}
	head, body, found := strings.Cut(content[4:], "\n---\n")
	if !found {
		return fields, content
	}
	for _, line := range strings.Split(head, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if ok && strings.TrimSpace(key) != "" {
			fields[strings.TrimSpace(key)] = strings.TrimSpace(value)
		}
	}
	return fields, body
}

// write shows the records: as JSON lines, with a format, or as their
// content. A command with save writes files instead, and shows only its
// format.
func (r *run) write(entries []*entry, out io.Writer) error {
	if r.command.Output.Save != nil {
		if err := r.save(entries); err != nil {
			return err
		}
	}
	w := bufio.NewWriter(out)
	defer w.Flush()

	records := make([]Record, len(entries))
	for i, e := range entries {
		records[i] = e.rec
	}
	if r.flags.JSON {
		encoder := json.NewEncoder(w)
		encoder.SetEscapeHTML(false)
		for _, record := range records {
			if err := encoder.Encode(record); err != nil {
				return err
			}
		}
		return nil
	}

	name := r.command.Output.Format
	if name == "" {
		if r.command.Output.Save != nil {
			return nil
		}
		for _, record := range records {
			text, ok := record["text"]
			if !ok {
				text = record["content"]
			}
			line(w, sanitize(str(text)))
		}
		return nil
	}
	return r.format(r.in.Manifest.Formats[name], records, w)
}

func (r *run) format(f Format, records []Record, w io.Writer) error {
	render := func(text string, s scope) error {
		if text == "" {
			return nil
		}
		t, err := compile(text, nil)
		if err != nil {
			return err
		}
		out, err := t.render(s, r, true)
		if err != nil {
			return err
		}
		line(w, out)
		return nil
	}

	if len(records) == 0 {
		return render(f.Empty, r.scope)
	}
	if err := render(f.Header, r.scope); err != nil {
		return err
	}
	for _, record := range records {
		s := r.recordScope(record)
		if f.Table != nil {
			table(w, scope(s).lookup(f.Table.Columns), scope(s).lookup(f.Table.Rows))
			continue
		}
		if err := render(f.Record, s); err != nil {
			return err
		}
	}
	return nil
}

// table writes rows as aligned columns, under a line of column names.
func table(w io.Writer, columns, rows any) {
	cell := func(v any) string {
		text := sanitize(str(v))
		return strings.NewReplacer("\t", " ", "\n", " ").Replace(text)
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if names, ok := columns.([]any); ok && len(names) > 0 {
		words := make([]string, len(names))
		for i, name := range names {
			words[i] = cell(name)
		}
		fmt.Fprintln(tw, strings.Join(words, "\t"))
	}
	list, _ := rows.([]any)
	for _, row := range list {
		values, _ := row.([]any)
		words := make([]string, len(values))
		for i, value := range values {
			words[i] = cell(value)
		}
		fmt.Fprintln(tw, strings.Join(words, "\t"))
	}
	tw.Flush()
}

// line writes text and ends it with a newline, when it has none.
func line(w io.Writer, text string) {
	// Text that renders to nothing shows nothing, not an empty line.
	if text == "" {
		return
	}
	io.WriteString(w, text)
	if !strings.HasSuffix(text, "\n") {
		io.WriteString(w, "\n")
	}
}

// Help lists the commands of a capability.
func Help(in Installed) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", in.Name, in.Manifest.Title)
	if in.Manifest.Summary != "" {
		fmt.Fprintf(&b, "%s\n", in.Manifest.Summary)
	}
	b.WriteString("\ncommands:\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
	for i := range in.Manifest.Commands {
		c := &in.Manifest.Commands[i]
		fmt.Fprintf(tw, "  %s\t%s\n", usage(in, c), c.Summary)
	}
	tw.Flush()
	return b.String()
}

func commandHelp(in Installed, c *Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\nusage: %s\n", c.Summary, usage(in, c))
	if len(c.Args) > 0 {
		b.WriteString("\narguments:\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 3, ' ', 0)
		for _, a := range c.Args {
			note := a.Type
			if a.Required {
				note += ", required"
			}
			if a.Default != "" {
				note += ", default " + a.Default
			}
			fmt.Fprintf(tw, "  %s\t%s\n", usageOf(a), note)
		}
		tw.Flush()
	}
	b.WriteString("\nevery command takes --json; a live call takes --later\n")
	return b.String()
}

func usage(in Installed, c *Command) string {
	words := append([]string{"arc", in.Name}, c.Path...)
	for _, a := range c.Args {
		text := usageOf(a)
		if !a.Required {
			text = "[" + text + "]"
		}
		words = append(words, text)
	}
	return strings.Join(words, " ")
}

// ShowReply shows the reply of a call by address, with the output of the
// service of the manifest. A reply that does not fit that output is written
// as it came, and the error says why.
func ShowReply(ctx context.Context, env Env, in Installed, body string, stdio Stdio) error {
	service := in.Manifest.Service
	if service == nil || service.Output == nil {
		return errors.New("the service has no output")
	}
	command := &Command{Output: *service.Output}
	r := &run{ctx: ctx, env: env, in: in, command: command, values: Values{}, stdio: stdio}
	entries, err := r.pipeline([]*entry{{rec: Record{"content": body}}})
	if err != nil {
		line(stdio.Out, body)
		return fmt.Errorf("the reply does not fit the output of %s: %w", in.Name, err)
	}
	if err := r.write(entries, stdio.Out); err != nil {
		return err
	}
	return r.exit(entries)
}
