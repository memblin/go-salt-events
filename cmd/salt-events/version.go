package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Build metadata, stamped by the linker at release time:
//
//	-ldflags "-X main.version=v0.1.0 -X main.commit=abc1234 -X main.buildDate=..."
//
// They are deliberately plain vars rather than constants: -X can only write to
// a string variable.
//
// The defaults are what a plain `go build` or `go install` produces, and they
// are NOT an error state — see buildInfo, which recovers the revision from the
// VCS stamps the toolchain embeds automatically. A binary someone installed
// with `go install` never sees these ldflags, and "unknown" for a build that is
// perfectly identifiable would be a worse answer than the one Go already has.
var (
	version   = "dev"
	commit    = ""
	buildDate = ""
)

// flagVersion is handled by splitVersionArg, not by config's FlagSet, for the
// same reason the capture flags are: a version query must answer even when the
// configuration is unloadable. Failing to report your own version because a
// socket is missing or a TOML file has a typo would be a bad joke — and it is
// the exact situation in which someone is asking.
const flagVersion = "version"

// splitVersionArg reports whether --version appears in args.
//
// It accepts the same spellings the flag package would (-version, --version),
// but deliberately not -version=true: a boolean flag with a value is a spelling
// nobody types, and accepting it would mean deciding what -version=false means.
func splitVersionArg(args []string) bool {
	for _, a := range args {
		if name, _, hasValue := splitFlag(a); name == flagVersion && !hasValue {
			return true
		}
	}

	return false
}

// buildInfo returns the version line, preferring linker stamps and falling back
// to the VCS information Go embeds in every module build.
//
// The fallback matters more than it looks: `go install
// github.com/TKC-Labs/go-salt-events/cmd/salt-events@latest` produces a binary
// with no ldflags at all, and without this it would report "dev" while the
// toolchain knew the exact commit the whole time.
func buildInfo() string {
	rev, dirty, goVer := readBuildInfo()

	v := version
	c := commit

	if c == "" {
		c = rev
	}

	if dirty && c != "" {
		c += "-dirty"
	}

	parts := make([]string, 0, 3)

	// Before any tag exists, `git describe --always` yields the bare commit, so
	// version and commit are the same string and printing both gives
	// "salt-events 2982588 (2982588, go1.26.6)". Say it once.
	if c != "" && !strings.Contains(v, c) {
		parts = append(parts, c)
	}

	if buildDate != "" {
		parts = append(parts, "built "+buildDate)
	}

	parts = append(parts, goVer)

	return fmt.Sprintf("salt-events %s (%s)", v, strings.Join(parts, ", "))
}

// readBuildInfo pulls the VCS revision, dirty flag and Go version out of the
// embedded build info. Everything is optional: a binary built outside a module,
// or with -buildvcs=false, simply has less to report.
func readBuildInfo() (rev string, dirty bool, goVer string) {
	goVer = runtime.Version()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "", false, goVer
	}

	if info.GoVersion != "" {
		goVer = info.GoVersion
	}

	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if len(s.Value) > 7 {
				rev = s.Value[:7]
			} else {
				rev = s.Value
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	return rev, dirty, goVer
}

// printVersion writes the version line. It goes to STDOUT and the caller exits
// 0, so `salt-events --version` is usable in a pipeline.
func printVersion(w io.Writer) error {
	_, err := fmt.Fprintln(w, buildInfo())

	return err
}
