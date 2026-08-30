package config_test

import (
	"errors"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/config"
)

func TestResolveSockDirReplicatesSaltsSunPathGuard(t *testing.T) {
	t.Parallel()

	// salt/config/__init__.py relocates sock_dir when it is more than 10
	// characters longer than cachedir, because sun_path is capped at 107
	// bytes. A resolver that ignores this looks in the wrong place on any
	// master where the guard fires, and reports "socket missing" on a
	// perfectly healthy host.
	tests := []struct {
		name       string
		configured string
		cachedir   string
		want       string
	}{
		{
			name:       "default layout does not relocate",
			configured: "/var/run/salt/master",
			cachedir:   "/var/cache/salt/master",
			want:       "/var/run/salt/master",
		},
		{
			name:       "long sock_dir relocates under cachedir",
			configured: "/var/run/salt/master/deeply/nested/socket/dir",
			cachedir:   "/var/cache/salt/master",
			want:       "/var/cache/salt/master/.salt-unix",
		},
		{
			name:       "boundary: exactly cachedir+10 does not relocate",
			configured: "/var/cache/salt/master0123456789",
			cachedir:   "/var/cache/salt/master",
			want:       "/var/cache/salt/master0123456789",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := config.ResolveSockDir(tt.configured, tt.cachedir); got != tt.want {
				t.Errorf("ResolveSockDir(%q, %q) = %q, want %q",
					tt.configured, tt.cachedir, got, tt.want)
			}
		})
	}
}

func TestSocketPathOnlyEverNamesThePublishSocket(t *testing.T) {
	t.Parallel()

	// Invariant 1: we must be structurally incapable of opening the pull
	// socket, because that is the one that could inject events onto the bus.
	got := config.SocketPath("/var/run/salt/master")

	if got != "/var/run/salt/master/master_event_pub.ipc" {
		t.Errorf("SocketPath = %q", got)
	}
}

func TestResolveConfigPathPrefersTheInvokingUsersHome(t *testing.T) {
	t.Parallel()

	// Under sudo, ~ is /root. A config written at the operator's own
	// ~/.config/salt-events/config.toml would be silently ignored, which is
	// indistinguishable from the config not working (spec §11).
	env := func(k string) string {
		switch k {
		case "SUDO_USER":
			return "tkcadmin"
		case "HOME":
			return "/root"
		default:
			return ""
		}
	}

	homeFor := func(user string) (string, error) {
		if user == "tkcadmin" {
			return "/home/tkcadmin", nil
		}

		return "", errors.New("no such user")
	}

	got, ok := config.ResolveConfigPath("", env, homeFor)
	if !ok {
		t.Fatal("ResolveConfigPath returned ok=false")
	}

	want := "/home/tkcadmin/.config/salt-events/config.toml"
	if got != want {
		t.Errorf("ResolveConfigPath = %q, want %q", got, want)
	}
}

func TestResolveConfigPathFallsBackWhenSudoUserIsUnknown(t *testing.T) {
	t.Parallel()

	env := func(k string) string {
		switch k {
		case "SUDO_USER":
			return "ghost"
		case "HOME":
			return "/root"
		default:
			return ""
		}
	}

	homeFor := func(string) (string, error) { return "", errors.New("no such user") }

	got, _ := config.ResolveConfigPath("", env, homeFor)

	want := "/root/.config/salt-events/config.toml"
	if got != want {
		t.Errorf("ResolveConfigPath = %q, want %q", got, want)
	}
}

func TestResolveConfigPathHonoursAnExplicitPath(t *testing.T) {
	t.Parallel()

	env := func(string) string { return "" }
	homeFor := func(string) (string, error) { return "", errors.New("unused") }

	got, ok := config.ResolveConfigPath("/etc/custom.toml", env, homeFor)
	if !ok || got != "/etc/custom.toml" {
		t.Errorf("ResolveConfigPath = %q, %v", got, ok)
	}
}
