package filter_test

import (
	"strings"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/filter"
	"github.com/TKC-Labs/go-salt-events/internal/model"
)

func ev(tag, minion, jid, fun string, retcode int, success bool, ns string, kind model.Kind) model.Event {
	return model.Event{
		Tag:       tag,
		Minion:    minion,
		JID:       jid,
		Fun:       fun,
		RetCode:   retcode,
		Success:   success,
		HasRet:    true,
		Namespace: ns,
		Kind:      kind,
	}
}

func TestParseAndMatch(t *testing.T) {
	t.Parallel()

	sample := ev(
		"salt/job/20260830081402123456/ret/scache-1",
		"scache-1", "20260830081402123456", "state.apply",
		1, false, "job", model.KindRet,
	)

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"empty query matches everything", "", true},
		{"exact tag glob", "salt/job/*/ret/*", true},
		{"non-matching tag glob", "salt/minion/*", false},
		{"prefix glob", "salt/job/*", true},
		{"minion field", "minion:scache-1", true},
		{"minion field mismatch", "minion:web-1", false},
		{"minion field glob", "minion:scache-*", true},
		{"jid field", "jid:20260830081402123456", true},
		{"fun field", "fun:state.apply", true},
		{"fun field glob", "fun:state.*", true},
		{"namespace field", "ns:job", true},
		{"kind field", "kind:ret", true},
		{"ok:false matches a failed return", "ok:false", true},
		{"ok:true does not match a failed return", "ok:true", false},
		{"terms AND together", "salt/job/*/ret/* minion:scache-1 ok:false", true},
		{"one failing term fails the whole query", "salt/job/*/ret/* minion:web-9", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := filter.Parse(tt.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.query, err)
			}

			if got := q.Match(sample); got != tt.want {
				t.Errorf("Parse(%q).Match() = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

func TestOkTrueMatchesASuccessfulReturn(t *testing.T) {
	t.Parallel()

	good := ev("salt/job/20260830081402123456/ret/web-1",
		"web-1", "20260830081402123456", "test.ping", 0, true, "job", model.KindRet)

	q, err := filter.Parse("ok:true")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !q.Match(good) {
		t.Error("ok:true did not match a successful return")
	}
}

func TestParseRejectsMalformedQueries(t *testing.T) {
	t.Parallel()

	// A malformed query must be reported, not silently treated as "match
	// nothing" — an empty pane during an incident reads as "no such events".
	for _, q := range []string{"ok:maybe", "kind:nonsense", "minion:", "[bad"} {
		if _, err := filter.Parse(q); err == nil {
			t.Errorf("Parse(%q) returned no error", q)
		}
	}
}

func TestIsZero(t *testing.T) {
	t.Parallel()

	q, err := filter.Parse("   ")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !q.IsZero() {
		t.Error("a whitespace-only query is not IsZero")
	}
}

func TestGlobsTreatSlashAsAnOrdinaryCharacter(t *testing.T) {
	t.Parallel()

	// Salt's fnmatch does not treat "/" as a separator. path.Match does, and
	// would make the single most common query in this tool silently match
	// nothing.
	sample := ev("salt/job/20260830081402123456/ret/scache-1",
		"scache-1", "20260830081402123456", "state.apply", 0, true, "job", model.KindRet)

	q, err := filter.Parse("salt/job/*")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if !q.Match(sample) {
		t.Error(`"salt/job/*" did not match a job return tag`)
	}
}

// TestGlobMetacharacters pins fnmatch semantics, including the two places this
// package follows Python (which Salt uses) rather than Go's path.Match.
func TestGlobMetacharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		query   string
		tag     string
		want    bool
		comment string
	}{
		{"star spans separators", "salt/*/ret/*", "salt/job/2026/ret/web-1", true, ""},
		{"star matches empty", "salt/auth*", "salt/auth", true, ""},
		{"leading star", "*ret/web-1", "salt/job/2026/ret/web-1", true, ""},
		{"question is exactly one rune", "salt/jo?", "salt/job", true, ""},
		{"question does not match empty", "salt/job?", "salt/job", false, ""},
		{"question matches one multi-byte rune", "salt/?", "salt/ü", true, ""},
		{"backtracking star", "*a*b*c", "xxaxxbxxc", true, ""},
		{"backtracking star that cannot finish", "*a*b*c", "xxaxxbxx", false, ""},
		{"class", "salt/minion/web-[0-9]/start", "salt/minion/web-7/start", true, ""},
		{"class miss", "salt/minion/web-[0-9]/start", "salt/minion/web-x/start", false, ""},
		{"class alternation", "salt/[jr]ob*", "salt/job/2026", true, ""},
		{"negated class uses ! like Python", "salt/[!j]*", "salt/run/2026", true, ""},
		{"negated class excludes", "salt/[!j]*", "salt/job/2026", false, ""},
		{
			"caret does not negate: [^j] is the class {^, j}",
			"salt/[^j]ob", "salt/job", true,
			"Go's path.Match would negate and reject this; Python's fnmatch, which Salt uses, makes a leading ^ a literal member",
		},
		{"caret class matches the caret itself", "salt/[^j]ob", "salt/^ob", true, ""},
		{"caret class matches nothing else", "salt/[^j]ob", "salt/xob", false, ""},
		{"star matches an empty subject", "*", "", true, ""},
		{"?* is how to require a non-empty subject", "?*", "", false, ""},
		{"class containing a closing bracket", "salt/[]x]", "salt/]", true, ""},
		{"trailing dash is literal", "web-[a-]", "web--", true, ""},
		{"backslash is an ordinary character", `salt\job`, `salt\job`, true, ""},
		{"dot is an ordinary character", "salt.job", "saltxjob", false, ""},
		{"matching is case sensitive", "SALT/*", "salt/job/2026", false, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := filter.Parse(tt.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.query, err)
			}

			got := q.Match(model.Event{Tag: tt.tag})
			if got != tt.want {
				t.Errorf("Parse(%q).Match(tag %q) = %v, want %v (%s)",
					tt.query, tt.tag, got, tt.want, tt.comment)
			}
		})
	}
}

// TestParseErrorsNameTheProblem checks not just that a bad query is rejected
// but that the message tells an operator which part of what they typed is
// wrong. The filter bar shows this text and nothing else.
func TestParseErrorsNameTheProblem(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		query    string
		wantText string
	}{
		{"unknown boolean", "ok:maybe", "maybe"},
		{"unknown kind", "kind:nonsense", "nonsense"},
		{"field with no value", "minion:", "minion"},
		{"unclosed class in a tag glob", "[bad", "[bad"},
		{"unclosed class in a field glob", "minion:web-[01", "web-[01"},
		{"unknown field", "cat:salt/job/*", "cat"},
		{"a lone colon", ":", `unknown field ""`},
		{"a bare glob containing a colon", "salt/job/*:x", "unknown field"},
		{"trailing colon after a good term", "minion:web-1 jid:", "jid"},
		{"only one bad term is needed", "salt/job/* ok:perhaps", "perhaps"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := filter.Parse(tt.query)
			if err == nil {
				t.Fatalf("Parse(%q) returned no error", tt.query)
			}

			if !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("Parse(%q) error = %q, want it to mention %q", tt.query, err, tt.wantText)
			}

			// A rejected query must not come back as a usable filter.
			if !q.IsZero() || q.String() != "" {
				t.Errorf("Parse(%q) returned a non-zero Query alongside its error", tt.query)
			}
		})
	}
}

// TestBareJIDTagsAreFilterable covers the six-of-32 real tags from a live Salt
// 3006.27 master whose entire tag is a JID, with no "salt/" prefix.
// internal/salttag files those under the custom namespace with an empty
// Category, so a query must still be able to reach them.
func TestBareJIDTagsAreFilterable(t *testing.T) {
	t.Parallel()

	// Shaped exactly as internal/salttag decodes it.
	pubAck := model.Event{
		Tag:       "20260830081402123456",
		Namespace: "custom",
		Category:  "",
		JID:       "20260830081402123456",
		Kind:      model.KindOther,
	}

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"bare tag glob reaches it", "2026*", true},
		{"match everything reaches it", "*", true},
		{"exact tag", "20260830081402123456", true},
		{"jid term reaches it", "jid:20260830081402123456", true},
		{"it is in the custom namespace", "ns:custom", true},
		{"it is not in the job namespace", "ns:job", false},
		{"an empty-namespace query does not match custom", "ns:job*", false},
		{"kind is other", "kind:other", true},
		// fnmatch's "*" matches the empty string, so "minion:*" is not a
		// has-a-minion test on an event that has none. "minion:?*" is.
		{"minion:* still matches, because * matches empty", "minion:*", true},
		{"minion:?* is how to demand an actual minion", "minion:?*", false},
		{"salt/ globs do not reach it", "salt/*", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := filter.Parse(tt.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.query, err)
			}

			if got := q.Match(pubAck); got != tt.want {
				t.Errorf("Parse(%q).Match(bare-JID event) = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestMinionIdentifierShapes: real minion ids carry hyphens and dots and are
// often FQDNs. None of those characters may be treated as syntax.
func TestMinionIdentifierShapes(t *testing.T) {
	t.Parallel()

	e := ev("salt/job/20260830081402123456/ret/scache-1.tkclabs.io",
		"scache-1.tkclabs.io", "20260830081402123456", "state.apply",
		0, true, "job", model.KindRet)

	tests := []struct {
		name  string
		query string
		want  bool
	}{
		{"exact fqdn", "minion:scache-1.tkclabs.io", true},
		{"domain suffix glob", "minion:*.tkclabs.io", true},
		{"host prefix glob", "minion:scache-*", true},
		{"dot is literal, not any-character", "minion:scache-1xtkclabs.io", false},
		{"hyphenated prefix", "minion:scache-?.tkclabs.io", true},
		{"tag glob over the same id", "*/ret/scache-1.tkclabs.io", true},
		{"a shorter id does not match", "minion:scache-1", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			q, err := filter.Parse(tt.query)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tt.query, err)
			}

			if got := q.Match(e); got != tt.want {
				t.Errorf("Parse(%q).Match() = %v, want %v", tt.query, got, tt.want)
			}
		})
	}
}

// TestOkOnlyAnswersForEventsThatCarriedAReturn is the invariant-10 shape of
// this package: never fabricate. "Did it succeed" is not a question a
// job/new or salt/auth event answers, so ok: must match neither way rather
// than counting the event as a failure.
func TestOkOnlyAnswersForEventsThatCarriedAReturn(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		event       model.Event
		wantOkTrue  bool
		wantOkFalse bool
	}{
		{
			name:        "job/new carries no return",
			event:       model.Event{Tag: "salt/job/2026/new", Kind: model.KindNew, HasRet: false},
			wantOkTrue:  false,
			wantOkFalse: false,
		},
		{
			name:        "auth carries no return",
			event:       model.Event{Tag: "salt/auth", Kind: model.KindAuth, HasRet: false},
			wantOkTrue:  false,
			wantOkFalse: false,
		},
		{
			name: "a ret-shaped event with no return field is still not an answer",
			// Reachable from a malformed or hand-fired salt/job/<jid>/ret/<id>
			// tag. Reading its zero-valued RetCode and Success would report a
			// failure that nothing on the bus ever claimed.
			event:       model.Event{Tag: "salt/job/2026/ret/web-1", Kind: model.KindRet, HasRet: false},
			wantOkTrue:  false,
			wantOkFalse: false,
		},
		{
			name: "a clean return is ok",
			event: model.Event{
				Tag: "salt/job/2026/ret/web-1", Kind: model.KindRet,
				HasRet: true, RetCode: 0, Success: true,
			},
			wantOkTrue:  true,
			wantOkFalse: false,
		},
		{
			name: "a non-zero retcode is not ok even when success is true",
			// spec §7.5: fail counts retcode != 0 OR success == false. A state
			// run with failing states reports success true, retcode 2.
			event: model.Event{
				Tag: "salt/job/2026/ret/web-1", Kind: model.KindRet,
				HasRet: true, RetCode: 2, Success: true,
			},
			wantOkTrue:  false,
			wantOkFalse: true,
		},
		{
			name: "success false is not ok even when retcode is zero",
			event: model.Event{
				Tag: "salt/job/2026/ret/web-1", Kind: model.KindRet,
				HasRet: true, RetCode: 0, Success: false,
			},
			wantOkTrue:  false,
			wantOkFalse: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			for query, want := range map[string]bool{"ok:true": tt.wantOkTrue, "ok:false": tt.wantOkFalse} {
				q, err := filter.Parse(query)
				if err != nil {
					t.Fatalf("Parse(%q): %v", query, err)
				}

				if got := q.Match(tt.event); got != want {
					t.Errorf("Parse(%q).Match() = %v, want %v", query, got, want)
				}
			}
		})
	}
}

// TestEveryKindIsQueryable guards the kind: table against model.Kind growing a
// value nobody taught the parser about. A new kind would otherwise render in
// the Live pane while "kind:<it>" reported "unknown kind".
func TestEveryKindIsQueryable(t *testing.T) {
	t.Parallel()

	const scanKinds = 32

	seen := map[string]bool{}

	for k := range model.Kind(scanKinds) {
		name := k.String()
		if seen[name] {
			continue
		}

		seen[name] = true

		q, err := filter.Parse("kind:" + name)
		if err != nil {
			t.Fatalf("Parse(kind:%s): %v — model.Kind gained a value the filter does not know", name, err)
		}

		if !q.Match(model.Event{Kind: k}) {
			t.Errorf("kind:%s did not match an event of that kind", name)
		}
	}
}

func TestKindDoesNotGlob(t *testing.T) {
	t.Parallel()

	// kind is a closed enum, so "kind:r*" is a typo, not a wildcard. Rejecting
	// it says so; accepting it as a literal would match nothing in silence.
	if _, err := filter.Parse("kind:r*"); err == nil {
		t.Error("Parse(kind:r*) returned no error")
	}
}

func TestStringAndZeroValue(t *testing.T) {
	t.Parallel()

	t.Run("String returns the trimmed input", func(t *testing.T) {
		t.Parallel()

		q, err := filter.Parse("  salt/job/*   minion:web-1  ")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if want := "salt/job/*   minion:web-1"; q.String() != want {
			t.Errorf("String() = %q, want %q", q.String(), want)
		}
	})

	t.Run("String re-parses to an equivalent query", func(t *testing.T) {
		t.Parallel()

		sample := ev("salt/job/2026/ret/web-1", "web-1", "2026", "test.ping",
			0, true, "job", model.KindRet)

		q, err := filter.Parse(" salt/job/*  minion:web-* ok:true ")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		again, err := filter.Parse(q.String())
		if err != nil {
			t.Fatalf("Parse(String()): %v", err)
		}

		if again.Match(sample) != q.Match(sample) || again.String() != q.String() {
			t.Error("String() does not round-trip through Parse")
		}
	})

	t.Run("the zero Query matches everything", func(t *testing.T) {
		t.Parallel()

		var q filter.Query

		if !q.IsZero() || q.String() != "" {
			t.Error("the zero Query is not zero")
		}

		if !q.Match(model.Event{}) || !q.Match(model.Event{Tag: "salt/auth"}) {
			t.Error("the zero Query did not match everything")
		}
	})

	t.Run("a query matches a zero Event without panicking", func(t *testing.T) {
		t.Parallel()

		q, err := filter.Parse("salt/* minion:web-1 ok:true kind:ret ns:job jid:2026 fun:test.ping")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if q.Match(model.Event{}) {
			t.Error("a fully constrained query matched the zero Event")
		}
	})
}

// TestQuotesAreOrdinaryCharacters: spec §6 says terms are space-separated and
// says nothing about quoting, so there is no quoting. An unclosed quote must
// therefore be a harmless literal rather than a parse failure or a panic — the
// filter bar sees every keystroke of a half-typed query.
func TestQuotesAreOrdinaryCharacters(t *testing.T) {
	t.Parallel()

	for _, query := range []string{`"salt/job/*`, `minion:"web-1`, `'`, `"`, `minion:'web 1'`} {
		q, err := filter.Parse(query)
		if err != nil {
			t.Errorf("Parse(%q) = %v, want a literal-quote term and no error", query, err)

			continue
		}

		// A quote is data, so it only matches a tag that really contains one.
		if q.Match(model.Event{Tag: "salt/job/2026", Minion: "web-1"}) {
			t.Errorf("Parse(%q) matched a tag with no quote in it", query)
		}
	}
}

// TestQueryBounds covers the two paste-sized inputs that would otherwise turn
// a filter into a frozen UI.
func TestQueryBounds(t *testing.T) {
	t.Parallel()

	t.Run("too many terms is an error, not a freeze", func(t *testing.T) {
		t.Parallel()

		if _, err := filter.Parse(strings.Repeat("salt/* ", 1000)); err == nil {
			t.Error("Parse of a 1000-term query returned no error")
		}
	})

	t.Run("a huge glob is an error, not a freeze", func(t *testing.T) {
		t.Parallel()

		if _, err := filter.Parse(strings.Repeat("*a", 5000)); err == nil {
			t.Error("Parse of a 10000-character glob returned no error")
		}
	})

	t.Run("an ordinary query is nowhere near the bounds", func(t *testing.T) {
		t.Parallel()

		q, err := filter.Parse("salt/job/*/ret/* minion:scache-1 fun:state.apply ok:false")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if q.IsZero() {
			t.Error("a four-term query parsed to nothing")
		}
	})

	t.Run("a pathological glob against a huge tag still terminates", func(t *testing.T) {
		t.Parallel()

		// Both sides are attacker-shaped: the glob is the largest this package
		// accepts, and the tag is the sort of thing event.send can put on the
		// bus (up to max_event_size). Matching is O(len(pattern)*len(tag)) with
		// no exponential blow-up, so this completes; if it ever regresses to
		// naive recursion, this test hangs the run instead of passing quietly.
		q, err := filter.Parse(strings.Repeat("*a", 256))
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}

		if q.Match(model.Event{Tag: strings.Repeat("b", 50_000)}) {
			t.Error("a pattern needing 256 a's matched a tag of b's")
		}
	})
}

// TestAdversarialInputNeverPanics: queries are typed interactively, so every
// partial and broken input below is reachable one keystroke at a time.
func TestAdversarialInputNeverPanics(t *testing.T) {
	t.Parallel()

	queries := []string{
		"", " ", "\t\n", ":", "::", ":::", "minion:", "minion::", ":minion",
		"[", "]", "[]", "[!", "[!]", "[]]", "[a-", "-", "*", "**", "***",
		"?", "*?[", "\x00", "\xff\xfe", "минион:web", "salt/😀/*",
		"ok:", "ok:TRUE", "kind:", "ns:", "jid:", "fun:", "минион",
		strings.Repeat(":", 100), strings.Repeat("[", 100), strings.Repeat("*", 100),
		"salt/job/*:*:*", "a b c d e f g h", "\\", "\\\\", "%", "$(rm -rf /)",
	}

	events := []model.Event{
		{},
		{Tag: "salt/auth"},
		{Tag: "20260830081402123456", Namespace: "custom", JID: "20260830081402123456"},
		{Tag: "salt/job/2026/ret/web-1.example.com", Namespace: "job", Minion: "web-1.example.com",
			JID: "2026", Fun: "state.apply", Kind: model.KindRet, HasRet: true, RetCode: 1},
		{Tag: "\xff\xfe", Minion: "\x00"},
		{Tag: strings.Repeat("x/", 5000)},
	}

	for _, query := range queries {
		q, err := filter.Parse(query)
		if err != nil {
			continue // Rejected, which is a fine outcome. It must not panic.
		}

		for _, e := range events {
			// The result is unconstrained here; not panicking or hanging is
			// the whole assertion.
			_ = q.Match(e)
		}

		if _, err := filter.Parse(q.String()); err != nil {
			t.Errorf("Parse(%q) accepted, but its String() %q did not re-parse: %v", query, q.String(), err)
		}
	}
}

// FuzzParse asserts the two properties the filter bar depends on: Parse never
// panics on anything a person can type, and a Query it accepts never panics
// when matched and always re-parses from its own String().
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"", "*", "salt/job/*/ret/*", "minion:web-1 ok:false", "[a-z]*",
		":", "[", "ok:maybe", "kind:ret", "ns:custom jid:2026",
	} {
		f.Add(seed)
	}

	sample := ev("salt/job/20260830081402123456/ret/web-1.example.com",
		"web-1.example.com", "20260830081402123456", "state.apply",
		1, false, "job", model.KindRet)

	f.Fuzz(func(t *testing.T, query string) {
		q, err := filter.Parse(query)
		if err != nil {
			return
		}

		got := q.Match(sample)

		again, err := filter.Parse(q.String())
		if err != nil {
			t.Fatalf("Parse(%q) succeeded but its String() %q did not: %v", query, q.String(), err)
		}

		if again.Match(sample) != got {
			t.Errorf("Parse(%q) and Parse(String()) disagree on the same event", query)
		}
	})
}
