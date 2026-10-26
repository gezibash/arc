package architecture

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTextOfOtherPartiesIsEscapedAtThePrint checks the rule of
// docs/clients/SPEC.md, section 3.9: text that another party chose reaches
// stdout, stderr or a log only through sdk/terminal. An error keeps the raw
// text of the other party, so the print escapes it, not the error.
//
// The check reads the source of each package. Each argument of a print
// that names an error or a field that another party fills, such as Message
// or ID, must be a call to sdk/terminal or to a function of the same
// package that returns the result of one. A print of text that arc writes
// itself carries the comment //arc:raw-ok and the reason on its line.
//
// The check reads names, not types or data flow. These still pass, until a
// check with go/types follows the wire types:
//   - an error under a name that the check does not know: it finds errors
//     by name, not by type, so fmt.Println(refused) passes;
//   - another party's text under a name that the check does not know,
//     such as x := reply.Message, then a print of x;
//   - text that reaches a print another way: a copy to a local variable,
//     fmt.Sprintf or + into a variable that is printed later, a helper
//     that prints, Write(buf.Bytes()) of a buffer, or a panic;
//   - a helper that returns raw and escaped text joined, such as
//     s + terminal.EscapeLine(x);
//   - a variation selector in a slog value: slog.TextHandler quotes each
//     other character that section 3.9 escapes, and keeps selectors.
//
// Five exceptions depend on their callers, and say so where they are:
// line in runtime/iface/run.go, the two prints of notebookIndex in
// runtime/iface/notebook.go, and the two writes of render in
// runtime/iface/template.go.
func TestTextOfOtherPartiesIsEscapedAtThePrint(t *testing.T) {
	root := filepath.Join("..", "..")
	for _, layer := range []string{"core", "sdk", "adapters", "runtime", "apps", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, layer), func(path string, entry fs.DirEntry, err error) error {
			if err != nil || !entry.IsDir() {
				return err
			}
			// The terminal package is the escape itself.
			if entry.Name() == "testdata" || filepath.ToSlash(path) == filepath.ToSlash(filepath.Join(root, "sdk", "terminal")) {
				return filepath.SkipDir
			}
			files := token.NewFileSet()
			var parsed []*ast.File
			matches, _ := filepath.Glob(filepath.Join(path, "*.go"))
			for _, name := range matches {
				if strings.HasSuffix(name, "_test.go") {
					continue
				}
				file, err := parser.ParseFile(files, name, nil, parser.ParseComments|parser.SkipObjectResolution)
				if err != nil {
					return err
				}
				parsed = append(parsed, file)
			}
			for _, found := range rawPrints(files, parsed) {
				file, _ := filepath.Rel(root, found.file)
				t.Errorf("%s:%d %s (docs/clients/SPEC.md, section 3.9)", filepath.ToSlash(file), found.line, found.text)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// TestTheEscapeCheckFindsRawPrints gives the check a package of hostile
// prints. It must find each line marked "want", and no other line.
func TestTheEscapeCheckFindsRawPrints(t *testing.T) {
	const source = `package hostile

import (
	"github.com/gezibash/arc/internal/atomicfile"
	lg "log"        // want
	logs "log/slog" // want
)

func old(s *server, e Entry, u Usage, m Message, w *strings.Builder, out io.Writer, logger *log.Logger, request Request, reply Reply) {
	fmt.Fprintf(s.log, "close of %.8s: %s\n", request.From, reply.Message) // want
	fmt.Printf("%s\n", e.ID)                                                // want
	fmt.Printf(", plan %s, %s\n", u.Plan, u.State)                          // want
	fmt.Println(e.Name)                                                     // want
	fmt.Fprintln(os.Stderr, "arc:", perr)                                   // want
	fmt.Fprintln(os.Stderr, "arc:", cause)                                  // want
	fmt.Fprintln(os.Stderr, "arc:", s.lastErr)                              // want
	fmt.Fprintln(os.Stderr, strings.TrimSpace(err.Error()))                 // want
	fmt.Println(launder(reply.Message))                                     // want
	fmt.Println(blank(reply.Message))                                       // want
	os.Stderr.Write([]byte(m.Text))                                         // want
	w.WriteString(reply.Reason)                                             // want
	io.WriteString(out, reply.Note)                                         // want
	logger.Printf("%s", reply.Title)                                        // want
	log.Fatal(err)                                                          // want
	log.Panicf("%v", err)                                                   // want
	slog.Info("refused", "reason", reply.Reason)                            // want
	_ = slog.New(slog.NewJSONHandler(os.Stderr, nil))                       // want
	slog.Default().Info("refused", "reason", refused.Message)               // want
	held := slog.Default()                                                  // want
	_ = slog.With("app", "storage")                                         // want
	j := slog.NewJSONHandler(os.Stderr, nil)
	_ = slog.New(j)                                                         // want
	fmt.Println(reply.Message) //arc:raw-ok
	// want above: a raw-ok with no reason

	fmt.Fprintf(s.log, "close of %.8s: %s\n", request.From, terminal.EscapeLine(reply.Message))
	fmt.Printf("%s\n", terminal.EscapeLine(e.ID))
	fmt.Println(errorLine(err))
	fmt.Println(shown(m.Text))
	fmt.Println(sanitize(m.Text))
	fmt.Fprintln(os.Stderr, "arc:", terminal.EscapeLine(perr.Error()))
	fmt.Fprintf(os.Stderr, "%d bytes\n", u.Used)
	fmt.Println(reply.Message) //arc:raw-ok the app wrote this message itself
	t.Fatal(err)
	_ = atomicfile.Write(path, []byte(reply.Message), 0600)
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("refused", "reason", reply.Reason)
	h := slog.NewTextHandler(os.Stderr, nil)
	_ = slog.New(h)
}

func launder(s string) string {
	if terminal.Blank(s) {
		return ""
	}
	return s
}

func blank(s string) string { return s + strconv.Itoa(terminal.Stripped(s)) }

func errorLine(err error) string { return "arc: " + terminal.EscapeLine(err.Error()) }

func shown(text string) string {
	return strings.ReplaceAll(terminal.Escape(text), "\n", "\n  ")
}

func sanitize(text string) string {
	clean := terminal.Strip(text)
	if clean != text {
		fmt.Fprintln(os.Stderr, "removed")
	}
	return clean
}
`
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "hostile.go", source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var want []int
	for i, line := range strings.Split(source, "\n") {
		if strings.HasSuffix(line, "// want") {
			want = append(want, i+1)
		}
		if strings.HasPrefix(strings.TrimSpace(line), "// want above") {
			want = append(want, i)
		}
	}
	var got []int
	for _, found := range rawPrints(files, []*ast.File{file}) {
		if !slices.Contains(got, found.line) {
			got = append(got, found.line)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		for _, found := range rawPrints(files, []*ast.File{file}) {
			t.Logf("line %d: %s", found.line, found.text)
		}
		t.Errorf("the check found lines %v, want %v", got, want)
	}
}

// The check lets a slog call through when its handler is slog.TextHandler,
// because that handler quotes each value. This proves the quoting.
func TestSlogTextHandlerQuotesHostileText(t *testing.T) {
	hostile := "a\x1b[2Jb\u202ec\u200bd\U000e0068e\nforged=1 f\u2028g\u0085h\u2066i"
	var out bytes.Buffer
	slog.New(slog.NewTextHandler(&out, nil)).Info(hostile, "reason", hostile, "error", errors.New(hostile))
	line := strings.TrimSuffix(out.String(), "\n")
	if strings.ContainsAny(line, "\x1b\u202e\u200b\U000e0068\n\u2028\u0085\u2066") {
		t.Errorf("slog.TextHandler wrote a hostile character: %q", line)
	}
	if strings.Count(line, `\x1b`) != 3 {
		t.Errorf("slog.TextHandler did not quote each value: %q", line)
	}
}

// printers are the functions and methods that write text for a person: the
// fmt, log and cobra prints, and the writes of a writer.
var printers = []string{
	"Print", "Printf", "Println", "Fprint", "Fprintf", "Fprintln",
	"PrintErr", "PrintErrf", "PrintErrln",
	"Fatal", "Fatalf", "Fatalln", "Panic", "Panicf", "Panicln",
	"Write", "WriteString",
}

// slogPrints are the functions of the slog package that log with the
// default handler, which the check cannot see. Default and With give a
// logger with that handler, so a logger that the code holds is caught
// where it is made.
var slogPrints = []string{
	"Debug", "Info", "Warn", "Error", "Log", "LogAttrs",
	"DebugContext", "InfoContext", "WarnContext", "ErrorContext",
	"Default", "With",
}

// loggers are the packages whose calls the check knows by name. An alias
// would hide them.
var loggers = []string{"log", "log/slog"}

// remoteNames are the names of fields and values that another party
// fills: a refusal, a reply, an entry of a list, an account, a message.
var remoteNames = []string{
	"Message", "Reason", "Status", "Note", "Title", "Summary", "Body",
	"Text", "Reply", "Code", "Error", "ID", "Plan", "State", "Name",
	"message", "reason", "status", "note", "title", "summary", "body",
	"text", "reply",
}

// escapes are the functions of sdk/terminal that give text that is safe to
// print.
var escapes = []string{"Escape", "EscapeLine", "EscapeLineCut", "EscapeLineData", "EscapeData", "EscapeJSON", "Strip"}

type rawPrint struct {
	file string
	line int
	text string
}

// rawPrints gives each printed argument in the files of one package that
// names another party's text with no escape, and each slog logger that
// does not quote its values.
func rawPrints(files *token.FileSet, parsed []*ast.File) []rawPrint {
	escapers := escaperFuncs(parsed)
	var found []rawPrint
	for _, file := range parsed {
		ok := rawOK(files, file)
		imports := importNames(file)
		handlers := textHandlers(file)
		add := func(node ast.Node, text string) {
			position := files.Position(node.Pos())
			reason, marked := ok[position.Line]
			if marked && reason != "" {
				return
			}
			if marked {
				text += "; //arc:raw-ok needs a reason"
			}
			found = append(found, rawPrint{position.Filename, position.Line, text})
		}
		for _, spec := range file.Imports {
			if path := strings.Trim(spec.Path.Value, `"`); spec.Name != nil && slices.Contains(loggers, path) {
				add(spec, "imports "+path+" as "+spec.Name.Name+"; the check knows it only by its name")
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, isCall := node.(*ast.CallExpr)
			if !isCall {
				return true
			}
			if isPkgCall(call, "slog", "New") && !textHandler(call, handlers) {
				add(call, "builds a slog logger whose handler is not slog.NewTextHandler")
				return true
			}
			if isPkgCall(call, "slog", slogPrints...) {
				add(call, "logs with the default slog handler, or holds a logger with it")
				return true
			}
			if !slices.Contains(printers, calledName(call)) || testReceiver(call) || fileWrite(call, imports) {
				return true
			}
			args := call.Args
			if len(args) > 0 && (strings.HasPrefix(calledName(call), "Fprint") || isPkgCall(call, "io", "WriteString")) {
				args = args[1:] // the writer
			}
			for _, arg := range args {
				if remote(arg, escapers) {
					var text strings.Builder
					printer.Fprint(&text, files, arg)
					add(arg, "prints "+text.String()+" without sdk/terminal")
				}
			}
			return true
		})
	}
	return found
}

// rawOK gives the lines of a file that carry //arc:raw-ok, with the reason
// after it.
func rawOK(files *token.FileSet, file *ast.File) map[int]string {
	lines := map[int]string{}
	for _, group := range file.Comments {
		for _, c := range group.List {
			if reason, ok := strings.CutPrefix(c.Text, "//arc:raw-ok"); ok {
				lines[files.Position(c.Pos()).Line] = strings.TrimSpace(reason)
			}
		}
	}
	return lines
}

// remote says whether an expression names an error or a field of another
// party outside a call to sdk/terminal or to an escaper.
func remote(expr ast.Expr, escapers map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			if escaping(n, escapers) {
				return false
			}
		case *ast.Ident:
			if errorName(n.Name) || slices.Contains(remoteNames, n.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

// errorName says whether a name is the name of an error: err, cause, or a
// name that ends in err or Err, such as perr or lastErr. Stderr is a
// writer.
func errorName(name string) bool {
	lower := strings.ToLower(name)
	return name == "cause" || (strings.HasSuffix(lower, "err") && !strings.HasSuffix(lower, "stderr"))
}

// escaping says whether a call gives escaped text: a call to an escape of
// sdk/terminal, or to an escaper.
func escaping(call *ast.CallExpr, escapers map[string]bool) bool {
	return isPkgCall(call, "terminal", escapes...) || escapers[calledName(call)]
}

// escaperFuncs gives the functions of a package that return the result of
// an escape. Each return of a value must give an escaped value or a
// literal, and one at least an escaped value. A local variable is escaped
// when each assignment to it is.
func escaperFuncs(parsed []*ast.File) map[string]bool {
	escapers := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, file := range parsed {
			for _, decl := range file.Decls {
				f, ok := decl.(*ast.FuncDecl)
				if ok && f.Body != nil && !escapers[f.Name.Name] && returnsEscaped(f.Body, escapers) {
					escapers[f.Name.Name] = true
					changed = true
				}
			}
		}
	}
	return escapers
}

func returnsEscaped(body *ast.BlockStmt, escapers map[string]bool) bool {
	contains := func(expr ast.Expr) bool {
		found := false
		ast.Inspect(expr, func(node ast.Node) bool {
			if call, ok := node.(*ast.CallExpr); ok && escaping(call, escapers) {
				found = true
			}
			return !found
		})
		return found
	}
	clean, dirty := map[string]bool{}, map[string]bool{}
	assign := func(name *ast.Ident, value ast.Expr) {
		if value != nil && contains(value) {
			clean[name.Name] = true
		} else {
			dirty[name.Name] = true
		}
	}
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if name, ok := lhs.(*ast.Ident); ok {
					var value ast.Expr
					if len(n.Rhs) == len(n.Lhs) {
						value = n.Rhs[i]
					}
					assign(name, value)
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				var value ast.Expr
				if len(n.Values) == len(n.Names) {
					value = n.Values[i]
				}
				assign(name, value)
			}
		}
		return true
	})
	escaped, all := false, true
	ast.Inspect(body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.FuncLit:
			return false
		case *ast.ReturnStmt:
			if len(n.Results) == 0 {
				return true
			}
			result := n.Results[0]
			name, isName := result.(*ast.Ident)
			switch {
			case contains(result) || (isName && clean[name.Name] && !dirty[name.Name]):
				escaped = true
			case isLiteral(result):
			default:
				all = false
			}
		}
		return true
	})
	return escaped && all
}

func isLiteral(expr ast.Expr) bool {
	_, ok := expr.(*ast.BasicLit)
	return ok
}

// textHandler says whether slog.New gets slog.NewTextHandler or
// slog.DiscardHandler, inline or through a variable of handlers.
func textHandler(call *ast.CallExpr, handlers map[string]bool) bool {
	if len(call.Args) != 1 {
		return false
	}
	switch arg := call.Args[0].(type) {
	case *ast.CallExpr:
		return isPkgCall(arg, "slog", "NewTextHandler")
	case *ast.SelectorExpr:
		return arg.Sel.Name == "DiscardHandler"
	case *ast.Ident:
		return handlers[arg.Name]
	}
	return false
}

// textHandlers gives the variables of a file that hold only a
// slog.NewTextHandler: each assignment to the name is such a call.
func textHandlers(file *ast.File) map[string]bool {
	text, other := map[string]bool{}, map[string]bool{}
	assign := func(name *ast.Ident, value ast.Expr) {
		if call, ok := value.(*ast.CallExpr); ok && isPkgCall(call, "slog", "NewTextHandler") {
			text[name.Name] = true
		} else {
			other[name.Name] = true
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				if name, ok := lhs.(*ast.Ident); ok && len(n.Rhs) == len(n.Lhs) {
					assign(name, n.Rhs[i])
				} else if ok {
					assign(name, nil)
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				var value ast.Expr
				if len(n.Values) == len(n.Names) {
					value = n.Values[i]
				}
				assign(name, value)
			}
		}
		return true
	})
	for name := range other {
		delete(text, name)
	}
	return text
}

// importNames gives the names of the packages that a file imports.
func importNames(file *ast.File) []string {
	var names []string
	for _, spec := range file.Imports {
		path := strings.Trim(spec.Path.Value, `"`)
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		names = append(names, name)
	}
	return names
}

// fileWrite says whether a call is the Write function of a package, such
// as atomicfile.Write: it writes a file, not to a terminal.
func fileWrite(call *ast.CallExpr, imports []string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "Write" {
		return false
	}
	x, ok := selector.X.(*ast.Ident)
	return ok && slices.Contains(imports, x.Name)
}

// testReceiver says whether a call is a method of t, tb or b: a test
// helper writes to the log of the test.
func testReceiver(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	x, ok := selector.X.(*ast.Ident)
	return ok && (x.Name == "t" || x.Name == "tb" || x.Name == "b")
}

// isPkgCall says whether call is pkg.name for one of names.
func isPkgCall(call *ast.CallExpr, pkg string, names ...string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !slices.Contains(names, selector.Sel.Name) {
		return false
	}
	x, ok := selector.X.(*ast.Ident)
	return ok && x.Name == pkg
}

// calledName gives the name of the called function or method.
func calledName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}
