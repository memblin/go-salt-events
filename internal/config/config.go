package config

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
)

// Defaults, all from spec §5.1, §7.5, and §10.
const (
	DefaultMaxMemory = 256 << 20 // 256 MiB
	DefaultMaxJobs   = 500
	DefaultExportMax = 1 << 30 // 1 GiB
	DefaultTheme     = "gruvbox-dark"
	RenderInterval   = 100 * time.Millisecond
)

// Flag names. They are constants because each one is referenced three times —
// once to register the flag, once to test whether the operator set it, and
// once in a diagnostic — and a typo in any of those would fail silently.
const (
	flagConfig    = "config"
	flagSockDir   = "sock-dir"
	flagTheme     = "theme"
	flagExportDir = "export-dir"
	flagFilter    = "filter"
	flagMaxMemory = "max-memory"
	flagExportMax = "export-max"
	flagMaxJobs   = "max-jobs"
	flagNoColor   = "no-color"
)

// Environment keys (spec §11: SALTEV_<KEY>).
const (
	envSockDir   = "SALTEV_SOCK_DIR"
	envTheme     = "SALTEV_THEME"
	envExportDir = "SALTEV_EXPORT_DIR"
	envFilter    = "SALTEV_FILTER"
	envMaxMemory = "SALTEV_MAX_MEMORY"
	envExportMax = "SALTEV_EXPORT_MAX"
	envMaxJobs   = "SALTEV_MAX_JOBS"
	envNoColor   = "SALTEV_NO_COLOR"
)

// ErrHelp is returned by Load when -h/--help was requested.
//
// The flag package has already printed the usage block by then, so this is a
// successful exit, not a failure: a caller that prints it as an error and
// exits non-zero makes `salt-events -h` look broken.
var ErrHelp = flag.ErrHelp

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

// Load resolves configuration with precedence flags > environment > config
// file > defaults (spec §11).
//
// env is injected for testability. Environment keys are SALTEV_<KEY>.
//
// Every tier fails loudly: a malformed environment value, an unparseable
// config file and an out-of-range budget all return an error naming what was
// wrong, because a setting that is silently ignored is indistinguishable from
// a setting that was never written (spec §8.1).
func Load(args []string, env func(string) string) (Config, error) {
	flags, explicitConfig, err := parseFlags(args)
	if err != nil {
		return Config{}, err
	}

	cfg := defaults()

	// The resolved path is recorded even when nothing is there, because §11
	// shows it in the help overlay: "which file would you read?" has to be
	// answerable without strace.
	path, _ := ResolveConfigPath(explicitConfig, env, homeForUser)
	cfg.ConfigPath = path

	fileCfg, err := readConfigFile(path)
	if err != nil {
		return Config{}, err
	}

	applyFile(&cfg, fileCfg)

	if err := applyEnv(&cfg, env); err != nil {
		return Config{}, err
	}

	applyFlags(&cfg, flags)

	// The sun_path guard second-guesses Salt's *default* layout. An operator
	// who named a sock_dir — in the file, the environment or on the command
	// line — did so precisely because auto-resolution pointed at the wrong
	// place, so relocating them anyway would silently defeat the override.
	if fileCfg.SockDir == nil && env(envSockDir) == "" && !flags.set[flagSockDir] {
		cfg.SockDir = ResolveSockDir(cfg.SockDir, DefaultCacheDir)
	}

	if err := validate(cfg); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// defaults is the lowest tier: what the tool does with no file, no
// environment and no flags.
func defaults() Config {
	return Config{
		SockDir:   DefaultSockDir,
		Theme:     DefaultTheme,
		MaxMemory: DefaultMaxMemory,
		ExportMax: DefaultExportMax,
		MaxJobs:   DefaultMaxJobs,
	}
}

// flagValues carries parsed flags alongside the set of flag names the operator
// actually passed.
//
// The distinction matters: flags are the *highest* tier, so a flag left at its
// default must not overwrite a value the file or the environment supplied.
// Seeding the FlagSet's defaults from the lower tiers would collapse that
// difference and also make "was --sock-dir given?" unanswerable.
type flagValues struct {
	cfg Config
	set map[string]bool
}

func parseFlags(args []string) (flagValues, string, error) {
	fset := flag.NewFlagSet("salt-events", flag.ContinueOnError)

	var explicitConfig string

	flags := flagValues{cfg: defaults(), set: make(map[string]bool)}

	fset.StringVar(&explicitConfig, flagConfig, "", "path to config.toml")
	fset.StringVar(&flags.cfg.SockDir, flagSockDir, flags.cfg.SockDir, "Salt master sock_dir")
	fset.StringVar(&flags.cfg.Theme, flagTheme, flags.cfg.Theme, "colour theme")
	fset.StringVar(&flags.cfg.ExportDir, flagExportDir, flags.cfg.ExportDir, "directory for NDJSON exports")
	fset.StringVar(&flags.cfg.Filter, flagFilter, flags.cfg.Filter, "initial filter query")
	fset.Int64Var(&flags.cfg.MaxMemory, flagMaxMemory, flags.cfg.MaxMemory, "event cache budget in bytes")
	fset.Int64Var(&flags.cfg.ExportMax, flagExportMax, flags.cfg.ExportMax, "maximum export size in bytes")
	fset.IntVar(&flags.cfg.MaxJobs, flagMaxJobs, flags.cfg.MaxJobs, "jobs retained in the correlation index")
	fset.BoolVar(&flags.cfg.NoColor, flagNoColor, flags.cfg.NoColor, "disable colour")

	if err := fset.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			// Usage has already been printed; -h is not a failure.
			return flags, "", ErrHelp
		}

		return flags, "", fmt.Errorf("parse flags: %w", err)
	}

	fset.Visit(func(f *flag.Flag) { flags.set[f.Name] = true })

	return flags, explicitConfig, nil
}

// applyFlags layers the flags the operator actually passed over everything
// below them.
func applyFlags(cfg *Config, flags flagValues) {
	if flags.set[flagSockDir] {
		cfg.SockDir = flags.cfg.SockDir
	}

	if flags.set[flagTheme] {
		cfg.Theme = flags.cfg.Theme
	}

	if flags.set[flagExportDir] {
		cfg.ExportDir = flags.cfg.ExportDir
	}

	if flags.set[flagFilter] {
		cfg.Filter = flags.cfg.Filter
	}

	if flags.set[flagMaxMemory] {
		cfg.MaxMemory = flags.cfg.MaxMemory
	}

	if flags.set[flagExportMax] {
		cfg.ExportMax = flags.cfg.ExportMax
	}

	if flags.set[flagMaxJobs] {
		cfg.MaxJobs = flags.cfg.MaxJobs
	}

	if flags.set[flagNoColor] {
		cfg.NoColor = flags.cfg.NoColor
	}
}

// applyEnv layers SALTEV_* over the config file, below flags.
//
// A malformed value is an error rather than a shrug: an operator who exported
// SALTEV_MAX_MEMORY=256MiB otherwise gets the default budget, no diagnostic,
// and cache shedding they cannot explain.
func applyEnv(cfg *Config, env func(string) string) error {
	if v := env(envSockDir); v != "" {
		cfg.SockDir = v
	}

	if v := env(envTheme); v != "" {
		cfg.Theme = v
	}

	if v := env(envExportDir); v != "" {
		cfg.ExportDir = v
	}

	if v := env(envFilter); v != "" {
		cfg.Filter = v
	}

	if err := envInt64(env, envMaxMemory, &cfg.MaxMemory); err != nil {
		return err
	}

	if err := envInt64(env, envExportMax, &cfg.ExportMax); err != nil {
		return err
	}

	if err := envInt(env, envMaxJobs, &cfg.MaxJobs); err != nil {
		return err
	}

	return envBool(env, envNoColor, &cfg.NoColor)
}

func envInt64(env func(string) string, key string, dst *int64) error {
	v := env(key)
	if v == "" {
		return nil
	}

	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%s=%q: %w", key, v, err)
	}

	*dst = n

	return nil
}

func envInt(env func(string) string, key string, dst *int) error {
	v := env(key)
	if v == "" {
		return nil
	}

	n, err := strconv.Atoi(v)
	if err != nil {
		return fmt.Errorf("%s=%q: %w", key, v, err)
	}

	*dst = n

	return nil
}

// envBool accepts exactly what strconv.ParseBool accepts: 1, t, T, TRUE,
// true, True and their false counterparts. Anything else is an error rather
// than a guess, so SALTEV_NO_COLOR=yes is reported instead of quietly leaving
// colour on.
func envBool(env func(string) string, key string, dst *bool) error {
	v := env(key)
	if v == "" {
		return nil
	}

	b, err := strconv.ParseBool(v)
	if err != nil {
		return fmt.Errorf("%s=%q: want a boolean (1/0, true/false): %w", key, v, err)
	}

	*dst = b

	return nil
}

// fileConfig mirrors the operator-settable subset of Config for TOML decoding.
//
// Every field is a pointer so "absent from the file" stays distinguishable
// from "present and set to the zero value"; otherwise a file that mentions
// nothing would blank every string setting below it.
type fileConfig struct {
	SockDir   *string `toml:"sock_dir"`
	Theme     *string `toml:"theme"`
	ExportDir *string `toml:"export_dir"`
	Filter    *string `toml:"filter"`

	MaxMemory *int64 `toml:"max_memory"`
	ExportMax *int64 `toml:"export_max"`
	MaxJobs   *int   `toml:"max_jobs"`

	NoColor *bool `toml:"no_color"`
}

// readConfigFile reads path as TOML.
//
// An absent file is not an error: resolution always ends at
// /etc/salt-events/config.toml, so "no config anywhere" is the ordinary case.
// A file that exists but does not parse IS an error, and it names the path
// (spec §8.1) — those two outcomes being indistinguishable is the same silent
// failure this package exists to prevent.
//
// It takes a path rather than resolving one, so a test can point it at a
// fixture without touching the real filesystem layout.
func readConfigFile(path string) (fileConfig, error) {
	var fileCfg fileConfig

	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fileCfg, nil
		}

		return fileCfg, fmt.Errorf("config unreadable at %s: %w", path, err)
	}

	if err := toml.Unmarshal(data, &fileCfg); err != nil {
		return fileCfg, fmt.Errorf("config unparseable at %s: %w", path, err)
	}

	return fileCfg, nil
}

// applyFile layers the config file over the defaults: the lowest of the three
// operator-facing tiers.
func applyFile(cfg *Config, fileCfg fileConfig) {
	if fileCfg.SockDir != nil {
		cfg.SockDir = *fileCfg.SockDir
	}

	if fileCfg.Theme != nil {
		cfg.Theme = *fileCfg.Theme
	}

	if fileCfg.ExportDir != nil {
		cfg.ExportDir = *fileCfg.ExportDir
	}

	if fileCfg.Filter != nil {
		cfg.Filter = *fileCfg.Filter
	}

	if fileCfg.MaxMemory != nil {
		cfg.MaxMemory = *fileCfg.MaxMemory
	}

	if fileCfg.ExportMax != nil {
		cfg.ExportMax = *fileCfg.ExportMax
	}

	if fileCfg.MaxJobs != nil {
		cfg.MaxJobs = *fileCfg.MaxJobs
	}

	if fileCfg.NoColor != nil {
		cfg.NoColor = *fileCfg.NoColor
	}
}

// validate rejects settings that would misbehave later rather than now.
func validate(cfg Config) error {
	if cfg.SockDir == "" {
		return errors.New(
			"sock-dir must not be empty: an empty sock_dir resolves the event socket " +
				"relative to the working directory",
		)
	}

	if cfg.MaxMemory <= 0 {
		return fmt.Errorf("max-memory must be positive, got %d", cfg.MaxMemory)
	}

	if cfg.MaxJobs <= 0 {
		return fmt.Errorf("max-jobs must be positive, got %d", cfg.MaxJobs)
	}

	// ExportMax is invariant 8's hard cap. A non-positive cap would leave the
	// export path guessing between "refuse everything" and "unlimited".
	if cfg.ExportMax <= 0 {
		return fmt.Errorf("export-max must be positive, got %d", cfg.ExportMax)
	}

	return nil
}

// homeForUser looks up a home directory in the passwd database.
func homeForUser(name string) (string, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return "", fmt.Errorf("lookup user %q: %w", name, err)
	}

	return u.HomeDir, nil
}
