package ui

// The global keys, named so the switch in root.go reads as intent rather than
// as literals and so goconst has nothing to count.
//
// keyPause is " " because bubbletea renders KeySpace as a single space; the
// filter editor relies on that too, where the same key is ordinary text.
const (
	keyQuit      = "q"
	keyInterrupt = "ctrl+c"
	keyNextPane  = "tab"
	keyPrevPane  = "shift+tab"
	keyTheme     = "t"
	keyPause     = " "
	keyFilter    = "/"
	keyHelp      = "?"
	keyExport    = "w"
	keyEscape    = "esc"
	keyEnter     = "enter"
	keyBackspace = "backspace"
)

// firstPaneDigit is '1' because the tab strip is numbered from one; pane zero
// would be the only thing on screen that is not.
const firstPaneDigit = '1'

// hints keeps the global keys permanently on screen. A read-only console is
// often someone's first contact with the tool during an incident, and a key
// you cannot see is a key you do not press. Every entry here is bound in
// handleKey: a hint strip that advertises a key nothing handles is worse than
// a shorter strip.
var hints = [...][2]string{
	{"1-5", "pane"},
	{keyNextPane, "next"},
	{keyTheme, "theme"},
	{keyFilter, "filter"},
	{"space", "pause"},
	{keyExport, "export"},
	{keyHelp, "help"},
	{keyQuit, "quit"},
}

// paneIndex maps a digit key onto a pane index, reporting false for anything
// else or for a digit past the last pane. Bounds are checked HERE rather than
// at the call site: "5" on a four-pane build is an ordinary keystroke, not a
// programming error, and must not index out of range.
func paneIndex(key string, n int) (int, bool) {
	if len(key) != 1 {
		return 0, false
	}

	i := int(key[0] - firstPaneDigit)
	if i < 0 || i >= n {
		return 0, false
	}

	return i, true
}
