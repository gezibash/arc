package architecture

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
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
// The check reads the source. Each argument of a print that names an error
// or a field that another party fills, such as Message or Status, must be
// a call to sdk/terminal or to a function of the same package that calls
// sdk/terminal. The few prints of text that arc writes itself are in
// arcText.
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
			for _, found := range rawPrints(t, path) {
				file, _ := filepath.Rel(root, found.file)
				file = filepath.ToSlash(file)
				if !slices.Contains(arcText[file], found.text) {
					t.Errorf("%s:%d prints %s without sdk/terminal (docs/clients/SPEC.md, section 3.9)", file, found.line, found.text)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

// arcText holds, for each file, the printed arguments that name an error or
// a field of another party, but hold only text that arc writes itself, or
// text that the function escaped before the print.
var arcText = map[string][]string{
	// The status of a conversation is "  new", "  <n> unread" or the
	// delivery state that arc-dm keeps.
	"apps/dm/cmd/arc-dm/main.go": {"status", "indent(text)"}, // text is terminal.Escape(m.Text)
	// The status is an error code or an account state of the app itself.
	"apps/storage/server/server.go": {"status"},
	// --raw writes the reply as it came, with a warning on a terminal;
	// each other case escapes it.
	"cmd/arc/capability.go": {"text"},
	// text is terminal.Escape or terminal.EscapeData of the record.
	"cmd/arc/session_http.go": {"text"},
	// The index goes to a buffer, and the record that holds it goes
	// through sanitize before the print.
	"runtime/iface/notebook.go": {"tableText(p.title)", "tableText(h.title)"},
	// Each caller of line passes text through sanitize or a template
	// that sanitizes.
	"runtime/iface/run.go": {"text"},
	// render cleans each part of a template that it shows; a template
	// that builds an event stays as it is.
	// TestTheTextOfAFormatCannotReachTheTerminal proves the shown case.
	"runtime/iface/template.go": {"text"},
}

// printers are the functions that write text for a person: the fmt, log and
// cobra prints.
var printers = []string{
	"Print", "Printf", "Println", "Fprint", "Fprintf", "Fprintln",
	"PrintErr", "PrintErrf", "PrintErrln", "Fatal", "Fatalf", "Fatalln",
	"WriteString",
}

// remoteFields are the names of fields and values that another party
// fills: a refusal, a reply, a message.
var remoteFields = []string{
	"Message", "Reason", "Status", "Note", "Title", "Summary", "Body",
	"Text", "Reply", "Code", "Error",
	"message", "reason", "status", "note", "title", "summary", "body",
	"text", "reply",
}

type rawPrint struct {
	file string
	line int
	text string
}

// rawPrints parses the production files of one directory, and gives each
// printed argument that names another party's text with no escape.
func rawPrints(t *testing.T, dir string) []rawPrint {
	t.Helper()
	files := token.NewFileSet()
	var parsed []*ast.File
	matches, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	for _, path := range matches {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		parsed = append(parsed, file)
	}
	escapers := map[string]bool{}
	for _, file := range parsed {
		for _, decl := range file.Decls {
			if f, ok := decl.(*ast.FuncDecl); ok && f.Body != nil && callsTerminal(f.Body) {
				escapers[f.Name.Name] = true
			}
		}
	}
	var found []rawPrint
	for _, file := range parsed {
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok || !slices.Contains(printers, calledName(call)) || testFatal(call) {
				return true
			}
			args := call.Args
			if len(args) > 0 && (strings.HasPrefix(calledName(call), "Fprint") || isCall(call, "io", "WriteString")) {
				args = args[1:] // the writer
			}
			for _, arg := range args {
				if remote(arg, escapers) {
					var text strings.Builder
					printer.Fprint(&text, files, arg)
					position := files.Position(arg.Pos())
					found = append(found, rawPrint{position.Filename, position.Line, text.String()})
				}
			}
			return true
		})
	}
	return found
}

// remote says whether an expression names an error or a field of another
// party outside a call to sdk/terminal or to an escaper.
func remote(expr ast.Expr, escapers map[string]bool) bool {
	found := false
	ast.Inspect(expr, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			if isTerminal(n.Fun) || escapers[calledName(n)] {
				return false
			}
		case *ast.Ident:
			if n.Name == "err" || strings.HasSuffix(n.Name, "Err") || slices.Contains(remoteFields, n.Name) {
				found = true
			}
		case *ast.SelectorExpr:
			if slices.Contains(remoteFields, n.Sel.Name) {
				found = true
			}
		}
		return !found
	})
	return found
}

func callsTerminal(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isTerminal(call.Fun) {
			found = true
		}
		return !found
	})
	return found
}

func isTerminal(fun ast.Expr) bool {
	selector, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return ok && pkg.Name == "terminal"
}

// testFatal says whether a call is t.Fatal or another Fatal that is not of
// the log package: a test helper writes to the log of the test.
func testFatal(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !strings.HasPrefix(selector.Sel.Name, "Fatal") {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	return !ok || pkg.Name != "log"
}

// isCall says whether call is pkg.name.
func isCall(call *ast.CallExpr, pkg, name string) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != name {
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
