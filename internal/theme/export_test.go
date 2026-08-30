package theme

// Compile re-exports compile to the external theme_test package.
//
// compile is unexported so that no package outside internal/theme can turn a
// hand-built Palette into a *Styles by any route — literal, zero value, or
// mutated copy (see styles.go). The theme package's own tests still need to
// compile every registered palette, so the standard export_test.go idiom
// hands them the unexported function without widening the shipped API: this
// file is compiled only under `go test`.
func Compile(p Palette) *Styles { return compile(p) }
