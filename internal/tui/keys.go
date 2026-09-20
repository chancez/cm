package tui

import (
	"strings"

	"charm.land/bubbles/v2/key"
)

// keyMap is the picker's own bindings, separate from the list's navigation and filter keys.
//
// A bubbles key.Map rather than a switch on strings, because the help line is then generated from the
// same bindings it describes: a key that moves and a help text that does not is a documentation bug
// that a reader blames on the tool.
type keyMap struct {
	Attach       key.Binding
	Switch       key.Binding
	Back         key.Binding
	New          key.Binding
	Kill         key.Binding
	Rename       key.Binding
	Preview      key.Binding
	HalfPageUp   key.Binding
	HalfPageDown key.Binding
	Help         key.Binding
	Quit         key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Attach: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "attach"),
		),
		// Switch moves the window that opened the picker rather than nesting an attachment inside it, which
		// is what enter would do from a session. Disabled unless the caller supplied a way to switch, so it
		// is absent from the help as well as inert: bubbles skips a disabled binding in both. "s" matches
		// the overlay's own switch key, which is where most people will arrive here from.
		Switch: key.NewBinding(
			key.WithKeys("s"),
			key.WithHelp("s", "switch here"),
			key.WithDisabled(),
		),
		// Back returns to the session that opened this picker, and carries no key of its own: the caller
		// names one and backBinding fills it in. Empty and disabled otherwise, which is the case that must
		// not offer a key -- run from a shell there is no client waiting, and the outer client intercepts
		// its prefix before this process could see it, so a binding here would describe a key that never
		// arrives.
		Back: key.NewBinding(key.WithDisabled()),
		New: key.NewBinding(
			key.WithKeys("n"),
			key.WithHelp("n", "new session"),
		),
		Kill: key.NewBinding(
			key.WithKeys("x"),
			key.WithHelp("x", "kill"),
		),
		Rename: key.NewBinding(
			key.WithKeys("r"),
			key.WithHelp("r", "rename"),
		),
		Preview: key.NewBinding(
			key.WithKeys("p"),
			key.WithHelp("p", "output"),
		),
		// Half a page, as in vim and less. The list already turns whole pages on u and d, from its own
		// KeyMap, so the control pair is what is missing rather than a second spelling of what is there:
		// ctrl-u and ctrl-d are half pages in every pager, and a session list is long enough to page
		// through and short enough that a whole page overshoots what was being looked at.
		HalfPageUp: key.NewBinding(
			key.WithKeys("ctrl+u"),
			key.WithHelp("ctrl+u", "half page up"),
		),
		HalfPageDown: key.NewBinding(
			key.WithKeys("ctrl+d"),
			key.WithHelp("ctrl+d", "half page down"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "keys"),
		),
		Quit: key.NewBinding(
			// esc is not bound. The list uses it to clear a filter, and a key that sometimes quits and
			// sometimes clears a filter quits at the wrong moment. ctrl-c is here because a terminal
			// program that ignores it is one people kill from another window.
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
	}
}

// ShortHelp is the one line under the list.
//
// Switch is not on it either, for the same measured reason: the line reached column 89 of 100 with the
// entries below, and "s switch here" takes it past the width. It is in the expanded help, in the column it
// belongs to rather than one of its own.
//
// The half page keys are not on it, and that is a space decision rather than an oversight: the line
// already carries the picker's actions plus the list's navigation and filter keys, it is not truncated
// to the window, and the layout measures the footer by counting newlines, so a line that wraps costs a
// row the list was given. Navigation belongs in the expanded help beside the page and jump keys anyway,
// which is where somebody looking for it looks.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Attach, k.New, k.Kill, k.Rename, k.Preview, k.Help, k.Quit}
}

// FullHelp is what "?" expands to.
//
// The list's own navigation and filter keys are added by the model, which owns the list and so is the
// only thing that can ask it what its bindings are, and the half page keys go into the list's own
// navigation column rather than into one of these. See model.fullHelp.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Attach, k.Switch, k.New},
		{k.Kill, k.Rename},
		// Back sits with quit because that is what it is a spelling of, and in the expanded help rather
		// than on the short line for the width reason above: the line already reaches column 89 of 100.
		// The startup notice says it instead, which is where somebody who has just pressed ctrl-] t is
		// looking anyway.
		{k.Preview, k.Help, k.Quit, k.Back},
	}
}

// backBinding is the caller's key for returning to the session it opened this picker from.
//
// spec is cm's spelling, "ctrl-<key>", which is what KeySpec.Name holds. Translated here rather than
// passed in bubbletea's form because the caller is a cm client talking about its own prefix key, and a
// command layer that had to know charm's key names to configure a picker would be knowing the wrong
// thing.
//
// A spec that is not a ctrl- combination leaves the binding disabled. The only caller passes a parsed
// KeySpec, so this covers "none", which is a prefix key the user turned off: with no way to open the
// overlay there is no way to arrive here, and offering the key anyway would be describing one that
// cannot be pressed.
func backBinding(spec string) key.Binding {
	k := teaKey(spec)
	if k == "" {
		return key.NewBinding(key.WithDisabled())
	}
	return key.NewBinding(
		key.WithKeys(k),
		key.WithHelp(spec, "back to the session"),
	)
}

// teaKey translates cm's spelling of an intercepted key into bubbletea's, or returns empty when the
// spec is not one.
//
// The two differ only in the separator, ctrl-] against ctrl+], and in nothing else that a cm spec can
// express: ParseKeySpec accepts a single character or a name that resolves to one byte, and bubbletea
// names ctrl plus NUL "ctrl+space", which is the same name cm took the byte from.
// TestTeaKeyMatchesBubbletea pairs the two so a rename upstream fails here rather than silently
// unbinding the key.
func teaKey(spec string) string {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(spec)), "ctrl-")
	if !ok || rest == "" {
		return ""
	}
	return "ctrl+" + rest
}
