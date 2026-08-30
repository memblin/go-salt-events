package main

import (
	"strings"
	"testing"

	"github.com/TKC-Labs/go-salt-events/internal/config"
)

// TestHelpDocumentsTheCaptureFlags joins two things that would otherwise drift
// apart: the flag names this package parses, and the usage text internal/config
// prints for -h.
//
// The capture flags are stripped from os.Args before config.Load runs, so they
// are not in config's FlagSet and the default Usage cannot know about them.
// They were undocumented in -h for exactly that reason. Naming the constants
// here means renaming one without the other fails a test instead of quietly
// producing help that omits a working flag.
func TestHelpDocumentsTheCaptureFlags(t *testing.T) {
	t.Parallel()

	// Bound to a variable rather than used inline: gocritic's argOrder heuristic
	// reads strings.Contains(<const>, <variable>) as reversed arguments.
	usage := config.UsageExtra

	for _, name := range []string{flagCapture, flagCaptureOut} {
		spelling := "-" + name

		if !strings.Contains(usage, spelling) {
			t.Errorf("config.UsageExtra does not document -%s, so it is absent from -h", name)
		}
	}
}
