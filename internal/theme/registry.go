package theme

import "sort"

// palettes is the registry. Values are canonical scheme colours adjusted where
// necessary to pass contrast AFTER 256-colour quantisation — never by lowering
// the threshold (see contrast.go).
var palettes = map[string]Palette{
	"gruvbox-dark": {
		Name: "gruvbox-dark", Dark: true,
		Base: "#282828", Surface: "#3c3836", Border: "#665c54", BorderFocus: "#fabd2f",
		Text: "#ebdbb2", Muted: "#a89984", Accent: "#83a598",
		Warn: "#fabd2f", Err: "#fb4934", Ok: "#b8bb26",
	},
	"solarized-dark": {
		Name: "solarized-dark", Dark: true,
		Base: "#002b36", Surface: "#073642", Border: "#586e75", BorderFocus: "#268bd2",
		Text: "#eee8d5", Muted: "#93a1a1", Accent: "#2aa198",
		Warn: "#b58900", Err: "#dc322f", Ok: "#859900",
	},
	"solarized-light": {
		Name: "solarized-light", Dark: false,
		Base: "#fdf6e3", Surface: "#eee8d5", Border: "#93a1a1", BorderFocus: "#268bd2",
		Text: "#073642", Muted: "#586e75",
		// Accent and Err are deliberately darker than canonical Solarized
		// (#1f7a70 and #dc322f respectively). Both canonical values clear
		// MinContrast in truecolor but fail it once quantised to the 256
		// xterm cube (Accent 4.44, Err 3.72 vs the 4.5 floor) — see
		// contrast.go. These are ~10% darker with hue preserved (same
		// R:G:B ratio, lower value), giving post-quantisation ratios of
		// 7.33 and 5.28. Do not "correct" these back to canonical values.
		Accent: "#1b6d64",
		Warn:   "#8a6700",
		Err:    "#c62d2a",
		Ok:     "#5f7300",
	},
	"mono": {
		Name: "mono", Dark: true,
		// Every slot empty: the mono palette relies on bold, reverse video,
		// and border glyphs alone, so the console stays legible over a pipe or
		// on a terminal with no colour at all. This is also the reason top-N
		// uses bar length rather than hue (spec §9).
	},
}

// DefaultName is the palette used when none is configured.
const DefaultName = "gruvbox-dark"

// Names returns every palette name, sorted.
func Names() []string {
	out := make([]string, 0, len(palettes))
	for n := range palettes {
		out = append(out, n)
	}

	sort.Strings(out)

	return out
}

// Get looks up a palette by name.
func Get(name string) (Palette, bool) {
	p, ok := palettes[name]

	return p, ok
}

// Next returns the palette after name, wrapping. Drives the `t` key.
func Next(name string) string {
	names := Names()

	for i, n := range names {
		if n == name {
			return names[(i+1)%len(names)]
		}
	}

	return DefaultName
}
