package salttag_test

import (
	"strings"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/model"
	"github.com/TKC-Labs/go-salt-events/internal/salttag"
)

func TestParse(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tag  string
		want salttag.Info
	}{
		{
			name: "job return",
			tag:  "salt/job/20260830081402123456/ret/scache-1",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/ret/*",
				JID:       "20260830081402123456",
				Minion:    "scache-1",
				Kind:      model.KindRet,
			},
		},
		{
			name: "job new",
			tag:  "salt/job/20260830081402123456/new",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/new",
				JID:       "20260830081402123456",
				Kind:      model.KindNew,
			},
		},
		{
			name: "state progress",
			tag:  "salt/job/20260830081402123456/prog/scache-1/3",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/prog/*/*",
				JID:       "20260830081402123456",
				Minion:    "scache-1",
				Kind:      model.KindProg,
			},
		},
		{
			name: "minion start",
			tag:  "salt/minion/scache-1/start",
			want: salttag.Info{
				Namespace: "minion",
				Category:  "salt/minion/*/start",
				Minion:    "scache-1",
				Kind:      model.KindStart,
			},
		},
		{
			name: "auth",
			tag:  "salt/auth",
			want: salttag.Info{
				Namespace: "auth",
				Category:  "salt/auth",
				Kind:      model.KindAuth,
			},
		},
		{
			name: "presence",
			tag:  "salt/presence/change",
			want: salttag.Info{
				Namespace: "presence",
				Category:  "salt/presence/change",
				Kind:      model.KindPresence,
			},
		},
		{
			name: "runner return",
			tag:  "salt/run/20260830081402123456/ret",
			want: salttag.Info{
				Namespace: "run",
				Category:  "salt/run/*/ret",
				JID:       "20260830081402123456",
				Kind:      model.KindRet,
			},
		},
		{
			// A custom event.send tag must not be forced into a salt
			// namespace, and must not explode cardinality either.
			name: "custom tag",
			tag:  "myapp/deploy/finished",
			want: salttag.Info{
				Namespace: "custom",
				Category:  "myapp/deploy/finished",
				Kind:      model.KindOther,
			},
		},
		{
			// Anything under salt/ that is not a known namespace still
			// normalises rather than being dropped.
			name: "unknown salt namespace",
			tag:  "salt/quux/thing",
			want: salttag.Info{
				Namespace: "custom",
				Category:  "salt/quux/thing",
				Kind:      model.KindOther,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := salttag.Parse(tt.tag)
			if got != tt.want {
				t.Errorf("Parse(%q)\n got = %+v\nwant = %+v", tt.tag, got, tt.want)
			}
		})
	}
}

// TestParseLiveTagShapes pins the tag shapes actually observed on a live Salt
// 3006.27 master (capture-report.md §2). Six of the 32 captured tags were bare
// JIDs with no salt/ prefix at all, so this is not an exotic edge case: it is
// roughly a fifth of the traffic produced by a plain `salt '*' test.ping`.
func TestParseLiveTagShapes(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		tag  string
		want salttag.Info
	}{
		{
			// The master's publish-ack to the CLI. Its whole tag is the JID,
			// so a parser that keys on it verbatim gets one Category per job —
			// exactly the cardinality explosion Category exists to prevent.
			name: "bare jid",
			tag:  "20260830145904725601",
			want: salttag.Info{
				Namespace: "job",
				Category:  "*",
				JID:       "20260830145904725601",
				Kind:      model.KindOther,
			},
		},
		{
			// Same event, different job: it must collapse to the same key.
			name: "bare jid other job",
			tag:  "20260830145909870073",
			want: salttag.Info{
				Namespace: "job",
				Category:  "*",
				JID:       "20260830145909870073",
				Kind:      model.KindOther,
			},
		},
		{
			// Fired by the master, unprefixed, once per minion. Keeping the
			// minion id in Category would leak one key per minion.
			name: "minion refresh",
			tag:  "minion/refresh/salt-1-tkclabs-io",
			want: salttag.Info{
				Namespace: "minion",
				Category:  "minion/refresh/*",
				Minion:    "salt-1-tkclabs-io",
				Kind:      model.KindOther,
			},
		},
		{
			name: "runner new",
			tag:  "salt/run/20260830145905341435/new",
			want: salttag.Info{
				Namespace: "run",
				Category:  "salt/run/*/new",
				JID:       "20260830145905341435",
				Kind:      model.KindNew,
			},
		},
		{
			// Minion ids in the wild carry hyphens; FQDN-style ids carry dots.
			name: "hyphenated minion id",
			tag:  "salt/job/20260830145905616937/ret/scache-2-tkclabs-io",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/ret/*",
				JID:       "20260830145905616937",
				Minion:    "scache-2-tkclabs-io",
				Kind:      model.KindRet,
			},
		},
		{
			name: "fqdn minion id",
			tag:  "salt/job/20260830145905616937/ret/scache-2.tkclabs.io",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/ret/*",
				JID:       "20260830145905616937",
				Minion:    "scache-2.tkclabs.io",
				Kind:      model.KindRet,
			},
		},
		{
			// salt/utils/jid.py appends "_<pid>" when unique_jid is enabled.
			// Treating that as a non-JID would put the raw jid in Category.
			name: "unique_jid suffix",
			tag:  "salt/job/20260830145905616937_31337/ret/scache-1",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/ret/*",
				JID:       "20260830145905616937_31337",
				Minion:    "scache-1",
				Kind:      model.KindRet,
			},
		},
		{
			name: "bare jid with unique_jid suffix",
			tag:  "20260830145905616937_31337",
			want: salttag.Info{
				Namespace: "job",
				Category:  "*",
				JID:       "20260830145905616937_31337",
				Kind:      model.KindOther,
			},
		},
		{
			// A bare token that is not a JID stays custom: only the JID shape
			// earns the job namespace.
			name: "bare non-jid token",
			tag:  "heartbeat",
			want: salttag.Info{
				Namespace: "custom",
				Category:  "heartbeat",
				Kind:      model.KindOther,
			},
		},
		{
			name: "key event",
			tag:  "salt/key",
			want: salttag.Info{
				Namespace: "key",
				Category:  "salt/key",
				Kind:      model.KindKey,
			},
		},
		{
			name: "wheel return",
			tag:  "salt/wheel/20260830145905616937/ret",
			want: salttag.Info{
				Namespace: "wheel",
				Category:  "salt/wheel/*/ret",
				JID:       "20260830145905616937",
				Kind:      model.KindRet,
			},
		},
		{
			// A job-shaped tag whose third segment is not a JID must not be
			// mistaken for one, and must not be starred out.
			name: "job namespace without a jid",
			tag:  "salt/job/notajid/ret/web-1",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/notajid/ret/web-1",
				Kind:      model.KindOther,
			},
		},
		{
			// Any minion can shape a tag. Trailing junk after the minion
			// segment must not become part of the aggregation key.
			name: "extra trailing segments on a return",
			tag:  "salt/job/20260830145905616937/ret/web-1/junk/more",
			want: salttag.Info{
				Namespace: "job",
				Category:  "salt/job/*/ret/*/*/*",
				JID:       "20260830145905616937",
				Minion:    "web-1",
				Kind:      model.KindRet,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := salttag.Parse(tt.tag)
			if got != tt.want {
				t.Errorf("Parse(%q)\n got = %+v\nwant = %+v", tt.tag, got, tt.want)
			}
		})
	}
}

func TestParseCategoryCollapsesCardinality(t *testing.T) {
	t.Parallel()

	// The whole point of Category: without JID and minion normalisation every
	// job would be its own top-N key and the Summary pane would be useless.
	a := salttag.Parse("salt/job/20260830081402123456/ret/web-1")
	b := salttag.Parse("salt/job/20260830999999999999/ret/web-2")

	if a.Category != b.Category {
		t.Errorf("categories differ: %q vs %q", a.Category, b.Category)
	}
}

func TestParseCategoryCollapsesBareJIDs(t *testing.T) {
	t.Parallel()

	// Six of 32 live frames were bare JIDs. One category per job here would
	// swamp every real category in the Summary pane.
	a := salttag.Parse("20260830145904725601")
	b := salttag.Parse("20260830145909870073")

	if a.Category != b.Category {
		t.Errorf("bare-JID categories differ: %q vs %q", a.Category, b.Category)
	}

	if a.JID == b.JID {
		t.Errorf("bare-JID JIDs must not collapse: both %q", a.JID)
	}
}

func TestParseDoesNotPanicOnDegenerateTags(t *testing.T) {
	t.Parallel()

	// Salt rejects empty tags at fire time, but never panic on bus data.
	degenerate := []string{
		"", "/", "salt", "salt/", "salt/job", "salt/job/",
		"//", "///", "salt//", "salt/job//ret", "salt/job///",
		"salt/minion", "salt/minion/", "salt/run/", "salt/wheel/",
		"minion/refresh", "minion/refresh/", "minion/",
		"salt/job/20260830081402123456/ret/",
		"salt/job/20260830081402123456/prog/",
		"20260830081402123456/", "/20260830081402123456",
	}

	for _, tag := range degenerate {
		_ = salttag.Parse(tag)
	}
}

func TestParseDoesNotPanicOnAbsurdTags(t *testing.T) {
	t.Parallel()

	// Tags come off the wire and any minion can shape one. A megabyte of
	// separators must yield a well-defined result, not a panic or a hang.
	for _, tag := range []string{
		strings.Repeat("/", 100000),
		strings.Repeat("a", 1<<20),
		"salt/job/" + strings.Repeat("9", 1<<16) + "/ret/x",
		strings.Repeat("salt/job/20260830081402123456/ret/x/", 10000),
	} {
		got := salttag.Parse(tag)
		if got.Namespace == "" {
			t.Errorf("Parse(<%d bytes>) left Namespace empty", len(tag))
		}
	}
}
