package secrets

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSecretsImportsOnlyTheStandardLibrary enforces §4.7's "Refuses to...
// Import anything outside the standard library" over every .go file in
// this package, including its own tests. A standard-library import path
// never carries a dot before its first slash (net/http, encoding/json,
// path/filepath); a module path always does, because it begins with a
// registrable domain.
// That is a reliable enough split to gate on: this package's own go.mod
// declares no dependencies at all, so any non-stdlib import here would be a
// compile failure before this test ever ran -- this test exists so a
// future edit gets a message that says why, rather than a bare "no
// required module provides package" from the toolchain.
func TestSecretsImportsOnlyTheStandardLibrary(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	fset := token.NewFileSet()
	found := false
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		found = true
		path := filepath.Join(dir, entry.Name())
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", entry.Name(), err)
		}
		for _, imp := range f.Imports {
			importPath := strings.Trim(imp.Path.Value, `"`)
			first, _, _ := strings.Cut(importPath, "/")
			if strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is not the standard library", entry.Name(), importPath)
			}
		}
	}
	if !found {
		t.Fatal("no .go files found in the current directory -- test is not checking anything")
	}
}
