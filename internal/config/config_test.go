package config_test

import (
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/config"
)

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
