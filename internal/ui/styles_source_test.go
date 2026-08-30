package ui_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// obtainers are the theme functions that hand out a palette or a style set.
// theme.Next is absent on purpose: it maps a name to a name and obtains
// nothing.
var obtainers = map[string]bool{
	"StylesFor": true,
	"Get":       true,
	"Compile":   true,
}

// rootFile is the one file allowed to call them.
const rootFile = "root.go"

// TestOnlyTheRootObtainsStyles asserts that the root model is the SOLE place
// in internal/ui (this package and every subpackage under it) that obtains a
// *theme.Styles. Panes receive one as a View parameter and never fetch their
// own.
//
// This is the half of the colour rule that no lint rule expresses. Unexporting
// theme.compile stops any package from compiling a hand-built palette, but a
// pane could still call theme.StylesFor itself — perfectly legal, contrast-
// validated, and still wrong: it would pin that pane to one palette, so
// pressing `t` would restyle every pane but that one. That reads as a
// half-broken theme rather than as a bug in one file, which is exactly the
// failure this project's theme contract exists to prevent (spec §3.2).
//
// It scans source rather than behaviour because the panes it governs
// (internal/ui/live, ui/rate, ui/summary, ui/jobs) are written by later tasks
// and cannot be imported from here.
func TestOnlyTheRootObtainsStyles(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	offenders, allowedCalls := []string{}, 0

	walk := func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk %s: %w", path, err)
		}

		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}

		for _, name := range themeCalls(file) {
			if path == rootFile {
				allowedCalls++

				continue
			}

			offenders = append(offenders, fmt.Sprintf("%s calls theme.%s", path, name))
		}

		return nil
	}

	if err := filepath.WalkDir(".", walk); err != nil {
		t.Fatalf("scanning internal/ui: %v", err)
	}

	// Without this the test would pass vacuously if the walk ever stopped
	// finding files — and a guard that cannot fail is worse than none.
	if allowedCalls == 0 {
		t.Fatalf("found no theme calls in %s at all; the scan is not working", rootFile)
	}

	for _, o := range offenders {
		t.Errorf("only the root model may obtain styles: %s", o)
	}
}

// themeCalls returns the names of the obtainer functions file calls on the
// theme package.
func themeCalls(file *ast.File) []string {
	var found []string

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}

		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "theme" || !obtainers[sel.Sel.Name] {
			return true
		}

		found = append(found, sel.Sel.Name)

		return true
	})

	return found
}
