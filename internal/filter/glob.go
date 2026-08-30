package filter

import "unicode/utf8"

// This file is the glob engine. It is deliberately not path.Match and not
// regexp:
//
//   - path.Match treats "/" as a separator, so "salt/job/*" would NOT match
//     "salt/job/<jid>/ret/<minion>". That is the single most common query this
//     tool will ever be given, and it would silently match nothing — the worst
//     possible failure for a filter, because an empty pane during an incident
//     reads as "there are no such events".
//   - regexp would force an operator to escape every "/" and "." in a tag,
//     which is the opposite of the muscle memory spec §6 is preserving.
//
// What is implemented is Python's fnmatch, because that is what Salt itself
// uses for reactor and beacon matching (fnmatch.fnmatch, case-sensitive on
// POSIX). Consequences of following Python rather than Go, all deliberate:
//
//   - "/" is an ordinary character.
//   - "!" negates a bracket expression; "^" does NOT (Python escapes a leading
//     "^" to a literal). Go's path.Match is the other way round.
//   - "\\" is an ordinary character. Python's fnmatch has no escape mechanism,
//     so neither does this. Salt tags contain no backslashes.
//
// The one place this deviates from Python is an unterminated "[": Python
// silently treats it as a literal, this package rejects it at parse time. See
// validateGlob.

// fnmatch reports whether s matches the compiled pattern p.
//
// p is pre-split into runes at parse time and s is walked in place, so a match
// allocates nothing: Match runs once per cached event per render, and an
// allocation here would be one per event per frame.
//
// The algorithm is the classic single-backtrack scan — remember where the last
// "*" was and how much it had absorbed, and on a mismatch let it absorb one
// more rune. That is O(len(p) * len(s)) worst case and has no exponential
// blow-up, which matters because both the pattern (typed or pasted by a person)
// and the subject (a tag, which a minion can make up to max_event_size long)
// are untrusted. parseTerm additionally caps the pattern length.
func fnmatch(p []rune, s string) bool {
	pi, si := 0, 0
	starP, starS := -1, 0

	for si < len(s) {
		c, width := utf8.DecodeRuneInString(s[si:])

		switch next, ok := matchOne(p, pi, c); {
		case ok:
			pi, si = next, si+width

		case pi < len(p) && p[pi] == '*':
			// Remember the star and let it absorb nothing for now.
			starP, starS = pi, si
			pi++

		case starP >= 0:
			// Backtrack: the last star absorbs one more rune.
			starS = nextRune(s, starS)
			pi, si = starP+1, starS

		default:
			return false
		}
	}

	// Trailing stars may match the empty remainder.
	for pi < len(p) && p[pi] == '*' {
		pi++
	}

	return pi == len(p)
}

// matchOne reports whether the pattern element at p[pi] matches the single
// rune c, and returns the index just past that element. A "*" never matches
// here — it is the caller's job, because it consumes no input.
func matchOne(p []rune, pi int, c rune) (int, bool) {
	if pi >= len(p) {
		return pi, false
	}

	switch p[pi] {
	case '*':
		return pi, false
	case '?':
		return pi + 1, true
	case '[':
		return matchClass(p, pi, c)
	default:
		return pi + 1, p[pi] == c
	}
}

// matchClass matches the bracket expression starting at p[pi] against c and
// returns the index just past its closing "]".
func matchClass(p []rune, pi int, c rune) (int, bool) {
	end := classEnd(p, pi)
	if end < 0 {
		// Unreachable after validateGlob, but an unterminated "[" must degrade
		// to a literal rather than index out of range: this runs on bus data.
		return pi + 1, p[pi] == c
	}

	i := pi + 1

	// Python's fnmatch negates with "!" only; a leading "^" is a literal.
	negated := i < end && p[i] == '!'
	if negated {
		i++
	}

	return end + 1, classHas(p[i:end], c) != negated
}

// classHas reports whether c is in the bracket expression's body (the runes
// between the optional "!" and the closing "]").
func classHas(body []rune, c rune) bool {
	for i := 0; i < len(body); i++ {
		// A "-" is a range only with a rune on each side; "[a-]" matches a
		// literal "-", as in Python.
		if i+2 < len(body) && body[i+1] == '-' {
			if body[i] <= c && c <= body[i+2] {
				return true
			}

			i += 2

			continue
		}

		if body[i] == c {
			return true
		}
	}

	return false
}

// classEnd returns the index of the "]" closing the bracket expression that
// starts at p[pi], or -1 if there is none.
//
// The scan is Python's: a "!" straight after the "[" is the negation marker,
// and a "]" straight after that is a literal member rather than the terminator,
// so "[]]" is the class containing "]".
func classEnd(p []rune, pi int) int {
	i := pi + 1

	if i < len(p) && p[i] == '!' {
		i++
	}

	if i < len(p) && p[i] == ']' {
		i++
	}

	for i < len(p) && p[i] != ']' {
		i++
	}

	if i >= len(p) {
		return -1
	}

	return i
}

// nextRune returns the index of the rune after the one at i.
//
// The guard is not decoration: DecodeRuneInString("") returns width 0, and a
// zero-width advance inside fnmatch's backtrack would spin forever. A hung TUI
// is no better than a panicked one, and both are reachable from bus data.
func nextRune(s string, i int) int {
	if i >= len(s) {
		return i + 1
	}

	_, width := utf8.DecodeRuneInString(s[i:])

	return i + width
}
