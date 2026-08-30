// Package salttag decodes a Salt event tag into its structured parts. It is
// pure and dependency-free so it can be shared unchanged by the TUI and, later,
// by the Prometheus exporter (spec §14).
package salttag

import (
	"strings"

	"github.com/TKC-Labs/go-salt-events/internal/model"
)

// Info is a decoded tag.
type Info struct {
	// Namespace is the second tag segment when it is one of Salt's ten
	// canonical namespaces (spec §2.8), otherwise "custom". Two unprefixed
	// shapes the master itself fires are classified too — see Parse.
	Namespace string

	// Category is the tag with JID and minion segments replaced by "*". It is
	// the aggregation key for every top-N view; without the normalisation each
	// job would be its own category.
	Category string

	JID    string
	Minion string
	Kind   model.Kind
}

// Namespace values and the tag segments that select them. These are consts
// rather than bare literals because each appears both as a namespaces key and
// as a switch case.
const (
	nsCustom   = "custom"
	nsAuth     = "auth"
	nsJob      = "job"
	nsKey      = "key"
	nsMinion   = "minion"
	nsSyndic   = "syndic"
	nsRun      = "run"
	nsWheel    = "wheel"
	nsPresence = "presence"
)

// namespaces is Salt's canonical set, from salt/utils/event.py::TAGS.
var namespaces = map[string]struct{}{
	nsAuth: {}, nsJob: {}, nsKey: {}, nsMinion: {}, nsSyndic: {},
	nsRun: {}, nsWheel: {}, "cloud": {}, "fileserver": {}, "queue": {},
	// Not in TAGS but fired by the master's presence system.
	nsPresence: {},
}

const (
	// jidDigits is the length of a Salt JID's timestamp part.
	jidDigits = 20

	starSeg = "*"
	saltSeg = "salt"

	// refreshSeg is the verb in the unprefixed minion/refresh/<id> tag the
	// master fires when a minion asks for a data cache refresh.
	refreshSeg = "refresh"

	// maxSegments caps how far a tag is split. Tags arrive off the bus and any
	// minion can shape one with event.send, so a tag of a million separators is
	// reachable; splitting it whole would allocate a million-element slice per
	// event. Everything past the cap lands in the final segment, which parses
	// to the same well-defined "unrecognised" result it would have anyway. Real
	// tags have at most six segments.
	maxSegments = 64
)

// Parse decodes tag. It never panics and never returns an error: a tag it does
// not recognise still yields a usable Category, because an unrecognised event
// is exactly the kind a person is looking for.
//
// Not every tag on a real bus is salt/-prefixed. A live capture of 32 frames
// from a Salt 3006.27 master showed six bare-JID tags (the master's publish-ack
// to the CLI, whose entire tag is the JID) and one minion/refresh/<id>. Both
// are handled explicitly: left as opaque custom tags they would each contribute
// one Category per job or per minion, which is the cardinality explosion
// Category exists to prevent.
func Parse(tag string) Info {
	segs := strings.SplitN(tag, "/", maxSegments)
	info := Info{Namespace: nsCustom, Kind: model.KindOther}

	norm := make([]string, len(segs))
	copy(norm, segs)

	if len(segs) >= 2 && segs[0] == saltSeg {
		parsePrefixed(segs, norm, &info)
	} else {
		parseUnprefixed(segs, norm, &info)
	}

	info.Category = strings.Join(norm, "/")

	return info
}

// parsePrefixed handles the canonical salt/<namespace>/... shapes.
func parsePrefixed(segs, norm []string, info *Info) {
	if _, ok := namespaces[segs[1]]; !ok {
		return
	}

	info.Namespace = segs[1]

	switch info.Namespace {
	case nsJob, nsRun, nsWheel:
		parseJobShaped(segs, norm, info)
	case nsMinion, nsSyndic:
		parseMinionShaped(segs, norm, info)
	case nsAuth:
		info.Kind = model.KindAuth
	case nsKey:
		info.Kind = model.KindKey
	case nsPresence:
		info.Kind = model.KindPresence
	}
}

// parseUnprefixed handles the two tag shapes the master fires without a salt/
// prefix. Anything else stays custom, per spec §2.8.
func parseUnprefixed(segs, norm []string, info *Info) {
	const (
		bareJIDSegs  = 1
		refreshSegs  = 3
		refreshIDIdx = 2
	)

	switch {
	case len(segs) == bareJIDSegs && isJID(segs[0]):
		// The master's publish-ack to the CLI: the whole tag is the JID, so
		// the whole tag normalises to "*". Namespace is job because that is
		// what the event is about — an operator filtering ns:job wants the
		// event carrying the job's expected-minion list.
		info.Namespace = nsJob
		info.JID = segs[0]
		norm[0] = starSeg

	case len(segs) == refreshSegs && segs[0] == nsMinion && segs[1] == refreshSeg:
		// minion/refresh/<id>, fired once per minion: the id must not become
		// part of the aggregation key.
		info.Namespace = nsMinion
		info.Minion = segs[refreshIDIdx]
		norm[refreshIDIdx] = starSeg
	}
}

// parseJobShaped handles salt/{job,run,wheel}/<jid>/... tags.
func parseJobShaped(segs, norm []string, info *Info) {
	const (
		jidIdx    = 2
		verbIdx   = 3
		minionIdx = 4
	)

	if len(segs) <= jidIdx || !isJID(segs[jidIdx]) {
		return
	}

	info.JID = segs[jidIdx]
	norm[jidIdx] = starSeg

	if len(segs) <= verbIdx {
		return
	}

	switch segs[verbIdx] {
	case "new":
		info.Kind = model.KindNew
	case "ret":
		info.Kind = model.KindRet

		takeMinion(segs, norm, info, minionIdx)
	case "prog":
		info.Kind = model.KindProg

		takeMinion(segs, norm, info, minionIdx)
	}
}

// takeMinion records the minion at idx and normalises it plus every segment
// after it. The trailing segments are unbounded — a progress run number, or
// junk on a hand-fired tag — so leaving them raw would put them in the
// aggregation key.
func takeMinion(segs, norm []string, info *Info, idx int) {
	if len(segs) <= idx {
		return
	}

	info.Minion = segs[idx]

	for i := idx; i < len(norm); i++ {
		norm[i] = starSeg
	}
}

// parseMinionShaped handles salt/{minion,syndic}/<id>/... tags.
func parseMinionShaped(segs, norm []string, info *Info) {
	const (
		idIdx   = 2
		verbIdx = 3
	)

	if len(segs) <= idIdx {
		return
	}

	info.Minion = segs[idIdx]
	norm[idIdx] = starSeg

	if len(segs) > verbIdx && segs[verbIdx] == "start" {
		info.Kind = model.KindStart
	}
}

// isJID reports whether s looks like a Salt JID: 20 digits, optionally followed
// by the "_<pid>" suffix salt/utils/jid.py appends when unique_jid is enabled.
func isJID(s string) bool {
	if len(s) < jidDigits || !allDigits(s[:jidDigits]) {
		return false
	}

	if len(s) == jidDigits {
		return true
	}

	suffix := s[jidDigits:]

	return suffix[0] == '_' && len(suffix) > 1 && allDigits(suffix[1:])
}

// allDigits reports whether s is non-empty and entirely ASCII digits.
func allDigits(s string) bool {
	if s == "" {
		return false
	}

	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}

	return true
}
