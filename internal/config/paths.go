// Package config resolves where things live and how settings compose.
//
// Both resolutions here exist because getting them wrong fails SILENTLY —
// the tool reports a missing socket on a healthy master, or ignores a config
// file that is sitting right there.
package config

import (
	"path/filepath"
)

// PubSocketName is the only socket this program ever opens.
//
// master_event_pull.ipc is deliberately absent from this package: the pull
// socket is the one that could inject events onto the bus, and invariant 1
// says we are structurally incapable of that. Do not add it.
const PubSocketName = "master_event_pub.ipc"

// DefaultSockDir matches Salt's own default.
const DefaultSockDir = "/var/run/salt/master"

// DefaultCacheDir matches Salt's master cachedir default.
const DefaultCacheDir = "/var/cache/salt/master"

// configRelPath is where a per-user config lives under a home directory.
var configRelPath = filepath.Join(".config", "salt-events", "config.toml")

// ResolveSockDir replicates salt/config/__init__.py:
//
//	if len(opts["sock_dir"]) > len(opts["cachedir"]) + 10:
//	    opts["sock_dir"] = os.path.join(opts["cachedir"], ".salt-unix")
//
// Salt does this because sun_path is limited to 107 bytes. A resolver that
// skips it looks in the configured directory on a master where Salt has
// already moved the socket somewhere else.
func ResolveSockDir(configured, cachedir string) string {
	const slack = 10

	if len(configured) > len(cachedir)+slack {
		return filepath.Join(cachedir, ".salt-unix")
	}

	return configured
}

// SocketPath returns the publish socket inside sockDir.
func SocketPath(sockDir string) string {
	return filepath.Join(sockDir, PubSocketName)
}

// ResolveConfigPath finds the config file.
//
// Under sudo, $HOME is root's, so a config at the operator's own
// ~/.config/salt-events/config.toml would never be read — and "my config is
// being ignored" is close to undiagnosable without strace. $SUDO_USER's home
// therefore wins over $HOME.
//
// env and homeFor are injected so this is testable without a real sudo
// environment or a real passwd database.
func ResolveConfigPath(
	explicit string,
	env func(string) string,
	homeFor func(string) (string, error),
) (string, bool) {
	if explicit != "" {
		return explicit, true
	}

	if u := env("SUDO_USER"); u != "" {
		if home, err := homeFor(u); err == nil && home != "" {
			return filepath.Join(home, configRelPath), true
		}
	}

	if home := env("HOME"); home != "" {
		return filepath.Join(home, configRelPath), true
	}

	return "/etc/salt-events/config.toml", true
}
