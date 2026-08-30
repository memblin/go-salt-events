package theme

import (
	"math"
	"strconv"
)

// MinContrast is WCAG AA for body text. Never lower it to make a palette pass
// — pick a different canonical colour and document why.
const MinContrast = 4.5

// Quantise256 snaps a truecolor hex value to the nearest colour in the xterm
// 256-colour cube.
//
// Contrast MUST be checked after this, not in truecolor: macOS Terminal.app is
// 256-colour only, and canonical scheme values that pass in truecolor can fail
// once quantised.
func Quantise256(c Color) Color {
	r, g, b, ok := parseHex(c)
	if !ok {
		return c
	}

	return Color("#" + hex2(snapCube(r)) + hex2(snapCube(g)) + hex2(snapCube(b)))
}

// cubeLevels are the six channel values of the xterm colour cube.
var cubeLevels = [6]int{0, 95, 135, 175, 215, 255}

// snapCube returns the nearest cube level to v.
func snapCube(v int) int {
	best, bestDist := cubeLevels[0], 1<<31-1

	for _, l := range cubeLevels {
		if d := abs(v - l); d < bestDist {
			best, bestDist = l, d
		}
	}

	return best
}

// ContrastRatio returns the WCAG contrast ratio between fg and bg.
func ContrastRatio(fg, bg Color) float64 {
	lf, lb := relLuminance(fg), relLuminance(bg)

	if lf < lb {
		lf, lb = lb, lf
	}

	return (lf + 0.05) / (lb + 0.05)
}

// relLuminance is WCAG relative luminance.
func relLuminance(c Color) float64 {
	r, g, b, ok := parseHex(c)
	if !ok {
		return 0
	}

	lin := func(v int) float64 {
		s := float64(v) / 255

		if s <= 0.04045 {
			return s / 12.92
		}

		return math.Pow((s+0.055)/1.055, 2.4)
	}

	return 0.2126*lin(r) + 0.7152*lin(g) + 0.0722*lin(b)
}

// parseHex reads "#rrggbb".
func parseHex(c Color) (r, g, b int, ok bool) {
	s := string(c)

	const hexLen = 7
	if len(s) != hexLen || s[0] != '#' {
		return 0, 0, 0, false
	}

	v, err := strconv.ParseUint(s[1:], 16, 32)
	if err != nil {
		return 0, 0, 0, false
	}

	return int(v>>16) & 0xff, int(v>>8) & 0xff, int(v) & 0xff, true
}

func hex2(v int) string {
	const base = 16

	s := strconv.FormatInt(int64(v), base)
	if len(s) == 1 {
		return "0" + s
	}

	return s
}

func abs(v int) int {
	if v < 0 {
		return -v
	}

	return v
}
