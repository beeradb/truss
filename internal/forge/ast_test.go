package forge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strconv"
	"strings"
	"testing"
)

// parseNonTestFiles parses every non-_test.go file in this package, the same
// convention internal/gates/gates_test.go uses: the guard is about what the
// production code does, not what a test happens to call.
func parseNonTestFiles(t *testing.T) (*token.FileSet, map[string]*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(info fs.FileInfo) bool {
		return !strings.HasSuffix(info.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing internal/forge: %v", err)
	}
	files := map[string]*ast.File{}
	for _, pkg := range pkgs {
		for name, f := range pkg.Files {
			files[name] = f
		}
	}
	return fset, files
}

// TestPullNumbersForCommitCannotCarryMerged is the load-bearing guard named
// in §4.6: `commits/{sha}/pulls` has no `merged` field, so
// PullNumbersForCommit must return `[]int` -- a type that has no room for one
// -- rather than a struct slice a later "convenience" change could grow a
// Merged field onto. This is checked structurally, over the AST, so the test
// fails even if someone changes the signature without renaming the mistake
// back into existence under a different method.
func TestPullNumbersForCommitCannotCarryMerged(t *testing.T) {
	_, files := parseNonTestFiles(t)

	var found *ast.FuncDecl
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != "PullNumbersForCommit" {
				continue
			}
			found = fn
		}
	}
	if found == nil {
		t.Fatal("no PullNumbersForCommit function declaration found")
	}
	if found.Recv == nil || len(found.Recv.List) != 1 {
		t.Fatalf("PullNumbersForCommit must be a method on *Client, found receiver %v", found.Recv)
	}

	results := found.Type.Results
	if results == nil || len(results.List) != 2 {
		t.Fatalf("PullNumbersForCommit must return exactly (?, error), found %v", results)
	}

	first := results.List[0].Type
	arr, ok := first.(*ast.ArrayType)
	if !ok || arr.Len != nil {
		t.Fatalf("PullNumbersForCommit's first return must be a slice type, found %T", first)
	}
	elt, ok := arr.Elt.(*ast.Ident)
	if !ok || elt.Name != "int" {
		t.Fatalf("PullNumbersForCommit must return []int, not []%s -- a named element type is exactly "+
			"the shape that could grow a Merged field", describeExpr(arr.Elt))
	}

	second, ok := results.List[1].Type.(*ast.Ident)
	if !ok || second.Name != "error" {
		t.Fatalf("PullNumbersForCommit's second return must be error, found %v", results.List[1].Type)
	}
}

func describeExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		return describeExpr(v.X) + "." + v.Sel.Name
	default:
		return "<expr>"
	}
}

// TestTheKeyNeverTouchesDisk enforces §4.6's refusal directly rather than
// trusting review: the GitHub App private key is bytes in memory only, and a
// call to os.WriteFile, os.Create or os.CreateTemp anywhere in this
// package's production code would put it in a file on disk -- which is the
// obvious convenience the moment anything wants to hand the key to a
// subprocess, and exactly the refusal §4.6 exists to hold.
func TestTheKeyNeverTouchesDisk(t *testing.T) {
	_, files := parseNonTestFiles(t)

	banned := map[string]bool{"WriteFile": true, "Create": true, "CreateTemp": true}

	for name, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, ok := sel.X.(*ast.Ident)
			if !ok || pkgIdent.Name != "os" {
				return true
			}
			if banned[sel.Sel.Name] {
				t.Errorf("%s calls os.%s: the private key must never touch disk", name, sel.Sel.Name)
			}
			return true
		})
	}
}

// TestGatesTypesAreUsedAsIs is a cheap sanity check that this package
// actually imports internal/gates rather than redefining shadow copies of
// its types (which would silently stop satisfying the gate functions).
func TestGatesTypesAreUsedAsIs(t *testing.T) {
	_, files := parseNonTestFiles(t)
	found := false
	for _, f := range files {
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if path == "github.com/beeradb/truss/internal/gates" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("internal/forge does not import internal/gates")
	}
}
