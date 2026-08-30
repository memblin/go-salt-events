package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/config"
)

func TestSplitVersionArgAcceptsBothSpellings(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		args []string
		want bool
	}{
		"single dash":         {[]string{"-version"}, true},
		"double dash":         {[]string{"--version"}, true},
		"among other flags":   {[]string{"--theme", "nord", "--version"}, true},
		"absent":              {[]string{"--theme", "nord"}, false},
		"not a prefix match":  {[]string{"--versions"}, false},
		"value form rejected": {[]string{"--version=true"}, false},
		"empty":               {nil, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if got := splitVersionArg(tc.args); got != tc.want {
				t.Errorf("splitVersionArg(%q) = %v, want %v", tc.args, got, tc.want)
			}
		})
	}
}

// TestVersionOutputCarriesTheGoVersion pins the one field that is always
// available. version/commit/buildDate are all empty or "dev" in a test binary —
// asserting on them here would only assert that the defaults are the defaults.
func TestVersionOutputCarriesTheGoVersion(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer

	if err := printVersion(&buf); err != nil {
		t.Fatalf("printVersion: %v", err)
	}

	got := buf.String()

	if !strings.HasPrefix(got, "salt-events ") {
		t.Errorf("version line does not start with the program name: %q", got)
	}

	if !strings.Contains(got, "go1.") {
		t.Errorf("version line omits the Go version, which is the first thing "+
			"wanted when chasing a runtime bug: %q", got)
	}

	if !strings.HasSuffix(got, "\n") {
		t.Errorf("version line is not newline-terminated, so it does not pipe cleanly: %q", got)
	}
}

// TestHelpDocumentsTheVersionFlag is the same rot-guard as the capture flags:
// --version is handled before config.Load, so config's FlagSet cannot know it
// exists and the default Usage would omit a working flag.
func TestHelpDocumentsTheVersionFlag(t *testing.T) {
	t.Parallel()

	usage := config.UsageExtra

	if !strings.Contains(usage, "-"+flagVersion) {
		t.Errorf("config.UsageExtra does not document -%s, so it is absent from -h", flagVersion)
	}
}
