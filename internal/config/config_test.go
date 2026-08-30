package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/config"
)

// noEnv is the empty environment: no SALTEV_*, no HOME, no SUDO_USER.
func noEnv(string) string { return "" }

// writeConfig puts a config.toml in a temp dir and returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	return path
}

func TestLoadPrecedenceFlagsBeatEnvironment(t *testing.T) {
	t.Parallel()

	env := func(k string) string {
		if k == "SALTEV_MAX_JOBS" {
			return "1000"
		}

		return ""
	}

	cfg, err := config.Load([]string{"--max-jobs=2000"}, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.MaxJobs != 2000 {
		t.Errorf("MaxJobs = %d, want 2000 (flag must beat env)", cfg.MaxJobs)
	}
}

func TestLoadEnvironmentBeatsDefaults(t *testing.T) {
	t.Parallel()

	env := func(k string) string {
		if k == "SALTEV_MAX_JOBS" {
			return "1000"
		}

		return ""
	}

	cfg, err := config.Load(nil, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.MaxJobs != 1000 {
		t.Errorf("MaxJobs = %d, want 1000", cfg.MaxJobs)
	}
}

func TestLoadRejectsNonPositiveBudgets(t *testing.T) {
	t.Parallel()

	env := func(string) string { return "" }

	if _, err := config.Load([]string{"--max-memory=0"}, env); err == nil {
		t.Error("expected an error for --max-memory=0")
	}

	if _, err := config.Load([]string{"--max-jobs=-1"}, env); err == nil {
		t.Error("expected an error for --max-jobs=-1")
	}
}

func TestLoadPrecedenceFlagsBeatEnvironmentBeatsFile(t *testing.T) {
	t.Parallel()

	// All three tiers disagree at once. Varying one source at a time would
	// pass even if the tiers were applied in the wrong order, so each case
	// here supplies every lower tier it claims to beat (spec §11:
	// flags > environment > config).
	path := writeConfig(t, "max_jobs = 100\ntheme = \"file-theme\"\n")

	envAll := func(k string) string {
		switch k {
		case "SALTEV_MAX_JOBS":
			return "1000"
		case "SALTEV_THEME":
			return "env-theme"
		default:
			return ""
		}
	}

	tests := []struct {
		name      string
		args      []string
		env       func(string) string
		wantJobs  int
		wantTheme string
	}{
		{
			name:      "flag beats environment beats file",
			args:      []string{"--config=" + path, "--max-jobs=2000", "--theme=flag-theme"},
			env:       envAll,
			wantJobs:  2000,
			wantTheme: "flag-theme",
		},
		{
			name:      "environment beats file when no flag is given",
			args:      []string{"--config=" + path},
			env:       envAll,
			wantJobs:  1000,
			wantTheme: "env-theme",
		},
		{
			name:      "file beats defaults when nothing else is set",
			args:      []string{"--config=" + path},
			env:       noEnv,
			wantJobs:  100,
			wantTheme: "file-theme",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(tt.args, tt.env)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if cfg.MaxJobs != tt.wantJobs {
				t.Errorf("MaxJobs = %d, want %d", cfg.MaxJobs, tt.wantJobs)
			}

			if cfg.Theme != tt.wantTheme {
				t.Errorf("Theme = %q, want %q", cfg.Theme, tt.wantTheme)
			}
		})
	}
}

func TestLoadWithAbsentConfigFileFallsBackToDefaults(t *testing.T) {
	t.Parallel()

	// "No config file" is the ordinary case — resolution always ends at
	// /etc/salt-events/config.toml, which usually is not there. It must not
	// be an error, or the tool refuses to start on a stock host.
	missing := filepath.Join(t.TempDir(), "absent.toml")

	cfg, err := config.Load([]string{"--config=" + missing}, noEnv)
	if err != nil {
		t.Fatalf("Load with an absent config: %v", err)
	}

	if cfg.MaxJobs != config.DefaultMaxJobs {
		t.Errorf("MaxJobs = %d, want the default %d", cfg.MaxJobs, config.DefaultMaxJobs)
	}

	if cfg.ConfigPath != missing {
		t.Errorf("ConfigPath = %q, want %q (the path is reported even when nothing is there)",
			cfg.ConfigPath, missing)
	}
}

func TestLoadRejectsAMalformedConfigFileAndNamesThePath(t *testing.T) {
	t.Parallel()

	// A present-but-unparseable file must be distinguishable from an absent
	// one, and the diagnostic must name the path tried (spec §8.1).
	path := writeConfig(t, "max_jobs = = 100\nthis is not toml\n")

	_, err := config.Load([]string{"--config=" + path}, noEnv)
	if err == nil {
		t.Fatal("expected an error for a malformed config file")
	}

	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name the path %q", err, path)
	}
}

func TestLoadRejectsMalformedEnvironmentValues(t *testing.T) {
	t.Parallel()

	// A typo'd env value used to be indistinguishable from an unset one: the
	// operator got the default and no diagnostic.
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{name: "byte suffix on max memory", key: "SALTEV_MAX_MEMORY", val: "256MiB"},
		{name: "letter O for zero in max jobs", key: "SALTEV_MAX_JOBS", val: "1O00"},
		{name: "non-integer export max", key: "SALTEV_EXPORT_MAX", val: "1GiB"},
		{name: "non-boolean no-color", key: "SALTEV_NO_COLOR", val: "yesplease"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			env := func(k string) string {
				if k == tt.key {
					return tt.val
				}

				return ""
			}

			_, err := config.Load(nil, env)
			if err == nil {
				t.Fatalf("expected an error for %s=%q", tt.key, tt.val)
			}

			if !strings.Contains(err.Error(), tt.key) {
				t.Errorf("error %q does not name %s", err, tt.key)
			}
		})
	}
}

func TestLoadAppliesTheRemainingEnvironmentSettings(t *testing.T) {
	t.Parallel()

	// Spec §11 applies the environment tier to all eight settings, not a
	// subset; export-max, filter and no-color used to be silently ignored.
	env := func(k string) string {
		switch k {
		case "SALTEV_EXPORT_MAX":
			return "4096"
		case "SALTEV_FILTER":
			return "tag:salt/job/*"
		case "SALTEV_NO_COLOR":
			return "true"
		default:
			return ""
		}
	}

	cfg, err := config.Load(nil, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.ExportMax != 4096 {
		t.Errorf("ExportMax = %d, want 4096", cfg.ExportMax)
	}

	if cfg.Filter != "tag:salt/job/*" {
		t.Errorf("Filter = %q, want %q", cfg.Filter, "tag:salt/job/*")
	}

	if !cfg.NoColor {
		t.Error("NoColor = false, want true")
	}
}

func TestLoadDoesNotOverrideAnExplicitSockDir(t *testing.T) {
	t.Parallel()

	// The sun_path guard used to fire against the hardcoded default cachedir
	// even when the operator had named a sock_dir — so an override supplied
	// *because* auto-resolution was wrong got silently redirected somewhere
	// else again, with no message.
	const long = "/srv/salt/run/master/sockets/here"

	fileWithSockDir := writeConfig(t, "sock_dir = \""+long+"\"\n")

	tests := []struct {
		name string
		args []string
		env  func(string) string
		want string
	}{
		{
			name: "explicit flag survives",
			args: []string{"--sock-dir=" + long},
			env:  noEnv,
			want: long,
		},
		{
			name: "explicit environment value survives",
			args: nil,
			env: func(k string) string {
				if k == "SALTEV_SOCK_DIR" {
					return long
				}

				return ""
			},
			want: long,
		},
		{
			name: "explicit config-file value survives",
			args: []string{"--config=" + fileWithSockDir},
			env:  noEnv,
			want: long,
		},
		{
			name: "the default is still guarded",
			args: nil,
			env:  noEnv,
			want: config.DefaultSockDir,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg, err := config.Load(tt.args, tt.env)
			if err != nil {
				t.Fatalf("Load: %v", err)
			}

			if cfg.SockDir != tt.want {
				t.Errorf("SockDir = %q, want %q", cfg.SockDir, tt.want)
			}
		})
	}
}

func TestLoadRejectsAnEmptySockDir(t *testing.T) {
	t.Parallel()

	// SocketPath("") is "master_event_pub.ipc": a lookup in the working
	// directory, which would produce a nonsense §8.1 diagnostic.
	if _, err := config.Load([]string{"--sock-dir="}, noEnv); err == nil {
		t.Error("expected an error for an empty --sock-dir")
	}
}

func TestLoadRejectsANonPositiveExportMax(t *testing.T) {
	t.Parallel()

	// ExportMax is invariant 8's hard cap; the export path must never have to
	// guess whether a negative cap means "refuse everything" or "unlimited".
	if _, err := config.Load([]string{"--export-max=-5"}, noEnv); err == nil {
		t.Error("expected an error for --export-max=-5")
	}
}

func TestLoadReportsHelpAsACleanExit(t *testing.T) {
	t.Parallel()

	// flag prints the usage block itself, so -h must be distinguishable from
	// a real failure or main prints an error line and exits 1 after it.
	_, err := config.Load([]string{"-h"}, noEnv)
	if !errors.Is(err, config.ErrHelp) {
		t.Errorf("Load(-h) = %v, want config.ErrHelp", err)
	}
}
