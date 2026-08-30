// Package filter parses and evaluates the event query language (spec §6).
//
// A query is space-separated terms that AND together. A bare term is a glob
// matched against the event's tag; a prefixed term narrows one field:
//
//	salt/job/*/ret/*  minion:scache-1  ok:false
//	     │                 │               └─ only failed returns
//	     │                 └─ field term
//	     └─ tag glob (fnmatch — Salt's own semantics)
//
// Globs are fnmatch, not path.Match and not regexp, because that is what Salt
// uses for reactor and beacon matching: an operator's muscle memory transfers
// directly instead of needing regex escaping for every "/" and ".". See glob.go
// for exactly which fnmatch, and why "/" must not be a separator.
//
// Terms match only the eagerly-extracted fields of an Event, never its payload,
// so filtering can never force a decode (spec §4.2, invariant 4). There is no
// cat: term: Category is an aggregation key for the Summary pane, and a bare
// term already globs the tag it was derived from.
//
// The first ":" in a term separates field from value, so a bare tag glob
// containing a ":" is read as a field term and rejected as an unknown field.
// That is a reported error rather than a silent mismatch, which is the trade
// this package makes everywhere: see Parse.
package filter

import (
	"errors"
	"fmt"
	"strings"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// Field prefixes, spec §6. These are consts rather than bare literals because
// each appears as a table key, as a switch case, and in a diagnostic.
const (
	// fieldTag is the empty prefix: a bare term, globbed against Event.Tag.
	fieldTag = ""

	fieldMinion = "minion"
	fieldJID    = "jid"
	fieldFun    = "fun"
	fieldOK     = "ok"
	fieldNS     = "ns"
	fieldKind   = "kind"
)

const (
	valueTrue  = "true"
	valueFalse = "false"

	// metachars is the set that makes a value a glob rather than a literal.
	metachars = "*?["

	// fieldList is the closed set of prefixes, for diagnostics. Spelt out
	// rather than derived from the map so the order is stable and readable.
	fieldList = "minion, jid, fun, ok, ns or kind"
)

// Bounds on a query. Both exist because the filter bar accepts a paste, not
// just typing, and both the term count and the pattern length multiply into
// per-event match cost: a query is evaluated once per cached event per render,
// so an accepted 500k-term paste would freeze the UI just as surely as a panic
// would kill it. The limits are far above anything a person types (real queries
// are one to five terms of a few dozen characters) and are rejected with a
// message rather than silently truncated. internal/salttag caps tag splitting
// for the same reason.
const (
	maxTerms     = 64
	maxGlobRunes = 512
)

// errUnclosedClass is returned for a glob with an unterminated "[".
//
// Python's fnmatch would treat that "[" as a literal. This package rejects it,
// because "[web-01" is far more likely to be a half-typed character class than
// a tag that really begins with a bracket, and the literal reading would match
// nothing while looking like a valid query.
var errUnclosedClass = errors.New(`unclosed "[" in glob`)

// validFields is the closed set of prefixed terms (spec §6).
var validFields = map[string]struct{}{
	fieldMinion: {}, fieldJID: {}, fieldFun: {},
	fieldOK: {}, fieldNS: {}, fieldKind: {},
}

// validKinds mirrors model.Kind's String values. A kind is a closed enum, so a
// typo is a query error rather than a term that matches nothing.
var validKinds = map[string]struct{}{
	"new": {}, "ret": {}, "prog": {}, "start": {},
	"auth": {}, "key": {}, "presence": {}, "other": {},
}

// term is one space-separated component of a query.
type term struct {
	// field is the prefix before ":", or fieldTag for a bare tag glob.
	field string

	// value is the term's text exactly as typed.
	value string

	// pattern is value pre-split into runes, or nil when value holds no
	// metacharacter and is therefore matched by equality. Compiling it once
	// here is what keeps Match allocation-free.
	pattern []rune
}

// Query is a compiled query. The zero Query matches everything.
type Query struct {
	terms []term
	raw   string
}

// Parse compiles s.
//
// Every term is validated at parse time so a typo surfaces in the filter bar
// immediately. A malformed query must never be evaluated as "match nothing":
// an empty pane during an incident reads as "there are no such events", which
// is a different and much worse message than "your query is wrong".
//
// On error the returned Query is the zero Query. That is match-everything, not
// match-nothing, so a caller that ignores the error still shows the operator
// every event rather than a blank pane — but callers should not ignore it: per
// spec §6 the previous filter stays active and the error is shown inline.
func Parse(s string) (Query, error) {
	fields := strings.Fields(s)

	if len(fields) > maxTerms {
		return Query{}, fmt.Errorf("query has %d terms, limit is %d", len(fields), maxTerms)
	}

	q := Query{
		raw:   strings.TrimSpace(s),
		terms: make([]term, 0, len(fields)),
	}

	for _, f := range fields {
		t, err := parseTerm(f)
		if err != nil {
			return Query{}, err
		}

		q.terms = append(q.terms, t)
	}

	return q, nil
}

// parseTerm compiles one term.
func parseTerm(f string) (term, error) {
	field, value, isPrefixed := strings.Cut(f, ":")

	if !isPrefixed {
		t, err := compileGlob(fieldTag, f)
		if err != nil {
			return term{}, fmt.Errorf("bad tag glob %q: %w", f, err)
		}

		return t, nil
	}

	if _, ok := validFields[field]; !ok {
		return term{}, fmt.Errorf("unknown field %q (want %s)", field, fieldList)
	}

	if value == "" {
		return term{}, fmt.Errorf("field %q has no value", field)
	}

	return parseFieldTerm(field, value)
}

// parseFieldTerm compiles the value of an already-validated field prefix.
func parseFieldTerm(field, value string) (term, error) {
	switch field {
	case fieldOK:
		if value != valueTrue && value != valueFalse {
			return term{}, fmt.Errorf("ok: wants %s or %s, got %q", valueTrue, valueFalse, value)
		}
	case fieldKind:
		if _, ok := validKinds[value]; !ok {
			return term{}, fmt.Errorf("unknown kind %q (want new, ret, prog, start, auth, key, presence or other)", value)
		}
	default:
		// minion, jid, fun and ns are globbed. Their value sets are data, not
		// syntax — a minion id or a custom namespace can be anything — so
		// there is nothing to validate beyond the glob itself.
		t, err := compileGlob(field, value)
		if err != nil {
			return term{}, fmt.Errorf("bad glob %q for %s: %w", value, field, err)
		}

		return t, nil
	}

	return term{field: field, value: value, pattern: nil}, nil
}

// compileGlob validates value as a glob and pre-splits it when it is one.
func compileGlob(field, value string) (term, error) {
	t := term{field: field, value: value, pattern: nil}

	if !strings.ContainsAny(value, metachars) {
		return t, nil
	}

	t.pattern = []rune(value)

	if len(t.pattern) > maxGlobRunes {
		return term{}, fmt.Errorf("glob is %d characters, limit is %d", len(t.pattern), maxGlobRunes)
	}

	if err := validateGlob(t.pattern); err != nil {
		return term{}, err
	}

	return t, nil
}

// validateGlob rejects a pattern this package will not match as written.
//
// Only bracket expressions can be malformed: "*" and "?" are valid anywhere,
// and there is no escape character to leave dangling.
func validateGlob(p []rune) error {
	for i := 0; i < len(p); i++ {
		if p[i] != '[' {
			continue
		}

		end := classEnd(p, i)
		if end < 0 {
			return fmt.Errorf("%w: %q", errUnclosedClass, string(p[i:]))
		}

		i = end
	}

	return nil
}

// IsZero reports whether the query constrains nothing, and so needs no
// filter-bar chrome and no per-event work.
func (q Query) IsZero() bool { return len(q.terms) == 0 }

// String returns the query text as typed, trimmed, for the filter bar. It
// re-parses to an equivalent Query.
func (q Query) String() string { return q.raw }

// Match reports whether e satisfies every term. Terms AND together.
func (q Query) Match(e model.Event) bool {
	for _, t := range q.terms {
		if !t.match(e) {
			return false
		}
	}

	return true
}

// match evaluates one term against e.
//
// Every field read here is one of the eagerly extracted ones, so filtering
// never forces a payload decode (spec §4.2, invariant 4) and a shed event
// still filters exactly as it did before its payload was dropped
// (spec §5.2, invariant 9).
func (t term) match(e model.Event) bool {
	switch t.field {
	case fieldTag:
		return t.globMatch(e.Tag)
	case fieldMinion:
		return t.globMatch(e.Minion)
	case fieldJID:
		return t.globMatch(e.JID)
	case fieldFun:
		return t.globMatch(e.Fun)
	case fieldNS:
		return t.globMatch(e.Namespace)
	case fieldKind:
		return e.Kind.String() == t.value
	case fieldOK:
		return okMatch(t.value == valueTrue, e)
	default:
		// Unreachable: parseTerm rejects every other field. Match nothing
		// rather than everything, so a future field added to the parser and
		// forgotten here cannot silently widen a query.
		return false
	}
}

// globMatch applies the term's pattern to s, or compares for equality when the
// term holds no metacharacter. The literal fast path is not just speed: it is
// most terms (minion:web-1, jid:2026...), and it never allocates or backtracks.
func (t term) globMatch(s string) bool {
	if t.pattern == nil {
		return t.value == s
	}

	return fnmatch(t.pattern, s)
}

// okMatch evaluates ok:true / ok:false.
//
// An event with no return at all matches neither: "did this succeed" is not a
// question a job/new or salt/auth event answers, and silently treating it as a
// failure would inflate every failure count on screen. HasRet is the gate
// because it records that the payload carried a "return" key at all, which is
// the same gate internal/export uses to decide whether retcode and success are
// meaningful enough to write out.
//
// Success itself is spec §7.5's definition, the one model.Job.Failed already
// counts by: a return is a failure if retcode != 0 OR success == false. A state
// run that applied cleanly but had failing states carries retcode 2, and this
// deliberately calls that not-ok.
func okMatch(want bool, e model.Event) bool {
	if !e.HasRet {
		return false
	}

	succeeded := e.RetCode == 0 && e.Success

	return want == succeeded
}
