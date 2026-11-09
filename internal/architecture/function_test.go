package architecture

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// maxFunctionLines is the most lines that a function may hold, with its
// signature and the function literals inside it. A longer function does
// more than one step: split it into steps with names.
const maxFunctionLines = 100

// TestFunctionSize refuses a function over maxFunctionLines, unless it is
// on functionDebt. The debt can only shrink: see checkFunctions.
func TestFunctionSize(t *testing.T) {
	sizes, err := functionSizes(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	for _, problem := range checkFunctions(sizes, functionDebt) {
		t.Error(problem)
	}
}

// checkFunctions compares the length of each function with the limit and
// with the debt. A function on the debt must hold exactly its listed
// length: a longer one grew, and a shorter one lowers its entry, so that
// it cannot grow back. An entry at or under the limit, or for a function
// that is gone, leaves the debt.
func checkFunctions(sizes, debt map[string]int) []string {
	var problems []string
	for name, n := range sizes {
		listed, ok := debt[name]
		switch {
		case !ok && n > maxFunctionLines:
			problems = append(problems, fmt.Sprintf("%s is %d lines, over %d: split it into steps", name, n, maxFunctionLines))
		case ok && n <= maxFunctionLines:
			problems = append(problems, fmt.Sprintf("%s is %d lines, at or under %d: remove it from functionDebt", name, n, maxFunctionLines))
		case ok && n > listed:
			problems = append(problems, fmt.Sprintf("%s grew from %d to %d lines: a listed function may only shrink", name, listed, n))
		case ok && n < listed:
			problems = append(problems, fmt.Sprintf("%s shrank from %d to %d lines: set its entry in functionDebt to %d", name, listed, n, n))
		}
	}
	for name := range debt {
		if _, ok := sizes[name]; !ok {
			problems = append(problems, fmt.Sprintf("%s is on functionDebt, but no such function exists: remove the entry", name))
		}
	}
	slices.Sort(problems)
	return problems
}

// functionSizes gives the length in lines of each function under root,
// by the name that functionName gives it. It reads the same files as
// TestFileSize.
func functionSizes(root string) (map[string]int, error) {
	sizes := map[string]int{}
	err := walkSources(root, func(path, relative string) error {
		files := token.NewFileSet()
		file, err := parser.ParseFile(files, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		maps.Copy(sizes, fileFunctionSizes(files, file, relative))
		return nil
	})
	return sizes, err
}

// fileFunctionSizes gives the length of each function in one parsed file:
// a declaration from the line of func to the line of its closing brace,
// and a package variable from its name to the end of its value when the
// value holds a function literal. When two have one name, such as two
// init functions, the longer one counts.
func fileFunctionSizes(files *token.FileSet, file *ast.File, path string) map[string]int {
	sizes := map[string]int{}
	add := func(name string, from, to token.Pos) {
		n := files.Position(to).Line - files.Position(from).Line + 1
		sizes[name] = max(sizes[name], n)
	}
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				add(functionName(path, d), d.Pos(), d.End())
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, v := range value.Values {
					if i < len(value.Names) && holdsFunction(v) {
						add(path+":"+value.Names[i].Name, value.Names[i].Pos(), v.End())
					}
				}
			}
		}
	}
	return sizes
}

// holdsFunction says whether an expression holds a function literal.
func holdsFunction(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		_, lit := n.(*ast.FuncLit)
		found = found || lit
		return !found
	})
	return found
}

// functionName names a function as path:Name, and a method as
// path:Type.Name, so that two methods of one name in one file differ.
func functionName(path string, fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return path + ":" + fn.Name.Name
	}
	return path + ":" + receiverType(fn.Recv.List[0].Type) + "." + fn.Name.Name
}

// receiverType gives the type name of a receiver: T for T, *T, T[K] and
// *T[K, V].
func receiverType(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverType(e.X)
	case *ast.IndexExpr:
		return receiverType(e.X)
	case *ast.IndexListExpr:
		return receiverType(e.X)
	case *ast.Ident:
		return e.Name
	}
	return fmt.Sprintf("%T", expr)
}

// TestCheckFunctions checks each rule of checkFunctions on a hand-made
// set of sizes.
func TestCheckFunctions(t *testing.T) {
	over := maxFunctionLines + 1
	cases := []struct {
		name  string
		sizes map[string]int
		debt  map[string]int
		want  string
	}{
		{"a function at the limit passes", map[string]int{"a.go:f": maxFunctionLines}, nil, ""},
		{"an unlisted function over the limit fails", map[string]int{"a.go:f": over}, nil, "a.go:f is 101 lines, over 100"},
		{"a listed function at its length passes", map[string]int{"a.go:f": 150}, map[string]int{"a.go:f": 150}, ""},
		{"a listed function that grew fails", map[string]int{"a.go:f": 151}, map[string]int{"a.go:f": 150}, "a.go:f grew from 150 to 151 lines"},
		{"a listed function that shrank must lower its entry", map[string]int{"a.go:f": 120}, map[string]int{"a.go:f": 150}, "a.go:f shrank from 150 to 120 lines"},
		{"a listed function at the limit leaves the list", map[string]int{"a.go:f": maxFunctionLines}, map[string]int{"a.go:f": 150}, "a.go:f is 100 lines, at or under 100"},
		{"an entry for a gone function leaves the list", nil, map[string]int{"a.go:f": 150}, "a.go:f is on functionDebt, but no such function exists"},
	}
	for _, c := range cases {
		got := strings.Join(checkFunctions(c.sizes, c.debt), "\n")
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

// TestFunctionSizeCounts counts a parsed file: a function from func to its
// closing brace, with its literals; each method under its type; the longer
// of two init functions; and a package variable that holds a function.
func TestFunctionSizeCounts(t *testing.T) {
	source := `package p

type T[K comparable] struct{}

// f has a doc comment, which does not count.
func f() {
	g := func() {
		_ = 1
	}
	g()
}

func (T[K]) m() {}

func (*U) m() {
}

type U struct{}

func init() {
	_ = 1
}

func init() {}

var command = &struct{ run func() }{
	run: func() {
		_ = 1
	},
}

var plain = 1
`
	files := token.NewFileSet()
	file, err := parser.ParseFile(files, "p.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	got := fileFunctionSizes(files, file, "p.go")
	want := map[string]int{"p.go:f": 6, "p.go:T.m": 1, "p.go:U.m": 2, "p.go:init": 3, "p.go:command": 5}
	for name, n := range want {
		if got[name] != n {
			t.Errorf("%s: got %d lines, want %d (all: %v)", name, got[name], n, got)
		}
	}
	if len(got) != len(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
