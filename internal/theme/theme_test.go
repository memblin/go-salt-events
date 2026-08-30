package theme_test

import (
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/theme"
)

func TestEveryPaletteCompilesAndIsFullyPopulated(t *testing.T) {
	t.Parallel()

	for _, name := range theme.Names() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, ok := theme.Get(name)
			if !ok {
				t.Fatalf("Get(%q) returned false", name)
			}

			if theme.Compile(p) == nil {
				t.Fatal("Compile returned nil")
			}

			if name == "mono" {
				return // mono is empty by design
			}

			slots := map[string]theme.Color{
				"Base": p.Base, "Surface": p.Surface, "Border": p.Border,
				"BorderFocus": p.BorderFocus, "Text": p.Text, "Muted": p.Muted,
				"Accent": p.Accent, "Warn": p.Warn, "Err": p.Err, "Ok": p.Ok,
			}

			for slot, c := range slots {
				if c == "" {
					t.Errorf("%s is empty", slot)
				}
			}
		})
	}
}

func TestContrastPassesAfter256ColourQuantisation(t *testing.T) {
	t.Parallel()

	// Checked AFTER quantisation because macOS Terminal.app is 256-colour
	// only, and canonical scheme values that pass in truecolor can fail once
	// snapped to the cube. Never lower MinContrast to make a palette pass —
	// pick a different canonical value and document it.
	for _, name := range theme.Names() {
		if name == "mono" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p, _ := theme.Get(name)

			base := theme.Quantise256(p.Base)

			for slot, fg := range map[string]theme.Color{
				"Text":   p.Text,
				"Muted":  p.Muted,
				"Accent": p.Accent,
				"Ok":     p.Ok,
				"Warn":   p.Warn,
				"Err":    p.Err,
			} {
				got := theme.ContrastRatio(theme.Quantise256(fg), base)
				if got < theme.MinContrast {
					t.Errorf("%s on Base = %.2f, want >= %.2f (after quantisation)",
						slot, got, theme.MinContrast)
				}
			}
		})
	}
}

func TestNextCyclesAndWraps(t *testing.T) {
	t.Parallel()

	names := theme.Names()
	seen := map[string]bool{}

	cur := names[0]
	for range len(names) {
		seen[cur] = true
		cur = theme.Next(cur)
	}

	if len(seen) != len(names) {
		t.Errorf("Next() visited %d of %d palettes", len(seen), len(names))
	}

	if cur != names[0] {
		t.Errorf("Next() did not wrap: ended at %q, want %q", cur, names[0])
	}
}
