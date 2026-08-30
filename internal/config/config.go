package config

import (
	"flag"
	"fmt"
	"os/user"
	"strconv"
	"time"
)

// Defaults, all from spec §5.1, §7.5, and §10.
const (
	DefaultMaxMemory = 256 << 20 // 256 MiB
	DefaultMaxJobs   = 500
	DefaultExportMax = 1 << 30 // 1 GiB
	DefaultTheme     = "gruvbox-dark"
	RenderInterval   = 100 * time.Millisecond
)

// Config is the resolved runtime configuration.
type Config struct {
	SockDir    string
	ConfigPath string
	ExportDir  string
	Theme      string
	Filter     string

	MaxMemory int64
	ExportMax int64
	MaxJobs   int

	NoColor bool
}

// Load resolves configuration with precedence flags > environment > defaults.
//
// env is injected for testability. Environment keys are SALTEV_<KEY>.
func Load(args []string, env func(string) string) (Config, error) {
	cfg := Config{
		SockDir:   DefaultSockDir,
		Theme:     DefaultTheme,
		MaxMemory: DefaultMaxMemory,
		ExportMax: DefaultExportMax,
		MaxJobs:   DefaultMaxJobs,
	}

	applyEnv(&cfg, env)

	fs := flag.NewFlagSet("salt-events", flag.ContinueOnError)
	explicitConfig := fs.String("config", "", "path to config.toml")
	fs.StringVar(&cfg.SockDir, "sock-dir", cfg.SockDir, "Salt master sock_dir")
	fs.StringVar(&cfg.Theme, "theme", cfg.Theme, "colour theme")
	fs.StringVar(&cfg.ExportDir, "export-dir", cfg.ExportDir, "directory for NDJSON exports")
	fs.StringVar(&cfg.Filter, "filter", cfg.Filter, "initial filter query")
	fs.Int64Var(&cfg.MaxMemory, "max-memory", cfg.MaxMemory, "event cache budget in bytes")
	fs.Int64Var(&cfg.ExportMax, "export-max", cfg.ExportMax, "maximum export size in bytes")
	fs.IntVar(&cfg.MaxJobs, "max-jobs", cfg.MaxJobs, "jobs retained in the correlation index")
	fs.BoolVar(&cfg.NoColor, "no-color", cfg.NoColor, "disable colour")

	if err := fs.Parse(args); err != nil {
		return Config{}, fmt.Errorf("parse flags: %w", err)
	}

	cfg.SockDir = ResolveSockDir(cfg.SockDir, DefaultCacheDir)

	path, _ := ResolveConfigPath(*explicitConfig, env, homeForUser)
	cfg.ConfigPath = path

	if cfg.MaxMemory <= 0 {
		return Config{}, fmt.Errorf("--max-memory must be positive, got %d", cfg.MaxMemory)
	}

	if cfg.MaxJobs <= 0 {
		return Config{}, fmt.Errorf("--max-jobs must be positive, got %d", cfg.MaxJobs)
	}

	return cfg, nil
}

// applyEnv layers SALTEV_* over the defaults, before flags.
func applyEnv(cfg *Config, env func(string) string) {
	if v := env("SALTEV_SOCK_DIR"); v != "" {
		cfg.SockDir = v
	}

	if v := env("SALTEV_THEME"); v != "" {
		cfg.Theme = v
	}

	if v := env("SALTEV_EXPORT_DIR"); v != "" {
		cfg.ExportDir = v
	}

	if v := env("SALTEV_MAX_MEMORY"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			cfg.MaxMemory = n
		}
	}

	if v := env("SALTEV_MAX_JOBS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.MaxJobs = n
		}
	}
}

// homeForUser looks up a home directory in the passwd database.
func homeForUser(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("lookup user %q: %w", name, err)
	}

	return u.HomeDir, nil
}
