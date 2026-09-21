package tui

import (
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"

	"github.com/chancez/cm/internal/keymap"
)

// keyMap is the picker's bindings, built from internal/keymap rather than written out here.
//
// A bubbles key.Map still, because the help line is then generated from the same bindings it describes: a
// key that moves and a help text that does not is a documentation bug that a reader blames on the tool.
// What changed is where the keys come from. They were literals here, so nothing could be rebound and the
// overlay's table and this one had no way to agree on a spelling.
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

// bindingsFrom turns a resolved keymap into the picker's bindings and the list's own.
//
// Both, because the list's navigation is as much a binding as the picker's verbs: bubbles/list holds its
// keys in a struct cm can overwrite, so leaving them alone would have made half the keys in this window
// configurable and the other half not, with nothing saying which was which.
func bindingsFrom(keys keymap.Map) (keyMap, list.KeyMap) {
	km := keyMap{
		Attach:       binding(keys, keymap.TUIAttach),
		Switch:       binding(keys, keymap.TUISwitch),
		Back:         binding(keys, keymap.TUIBack),
		New:          binding(keys, keymap.TUINew),
		Kill:         binding(keys, keymap.TUIKill),
		Rename:       binding(keys, keymap.TUIRename),
		Preview:      binding(keys, keymap.TUIOutput),
		HalfPageUp:   binding(keys, keymap.TUIHalfPageUp),
		HalfPageDown: binding(keys, keymap.TUIHalfPageDown),
		Help:         binding(keys, keymap.TUIHelp),
		Quit:         binding(keys, keymap.TUIQuit),
	}

	// The list's defaults are replaced rather than edited, so what is here is the whole of what its
	// navigation does. The filtering keys it owns are left as bubbles set them: accepting or cancelling a
	// filter is part of its text field rather than a cm action, and esc already reaches it as clear-filter.
	lk := list.DefaultKeyMap()
	lk.CursorUp = binding(keys, keymap.TUIUp)
	lk.CursorDown = binding(keys, keymap.TUIDown)
	lk.PrevPage = binding(keys, keymap.TUIPageUp)
	lk.NextPage = binding(keys, keymap.TUIPageDown)
	lk.GoToStart = binding(keys, keymap.TUIStart)
	lk.GoToEnd = binding(keys, keymap.TUIEnd)
	lk.Filter = binding(keys, keymap.TUIFilter)
	lk.ClearFilter = binding(keys, keymap.TUIClearFilter)
	// The list's own quit keys are cleared rather than bound, because the model handles quitting before it
	// delegates anything to the list. Left in place they were inert and still rendered: bubbles' default
	// binds Quit to "v" with the help text "select", so every picker help line carried a "v select" entry
	// for a key that did nothing, and a user who bound v to one of their own actions would see both.
	lk.Quit = key.NewBinding(key.WithDisabled())
	lk.ForceQuit = key.NewBinding(key.WithDisabled())
	return km, lk
}

// binding is one action as bubbles sees it.
//
// An action with no keys is disabled rather than bound to nothing, which is what bubbles wants: key.Matches
// skips a disabled binding and the help leaves it out, so an action someone unbound with an empty list is
// absent from both rather than listed with a blank key.
//
// The help shows the first key and the full label. Every key would widen the one line the picker has room
// for: the measured limit is column 89 of 100 with the defaults, and `cm keys` is where the whole set is.
func binding(keys keymap.Map, action keymap.Action) key.Binding {
	teaKeys := keys.TeaKeys(action)
	if len(teaKeys) == 0 {
		return key.NewBinding(key.WithDisabled())
	}
	return key.NewBinding(
		key.WithKeys(teaKeys...),
		key.WithHelp(keys.First(action), keys.Label(action)),
	)
}

// ShortHelp is the one line under the list.
//
// Switch and Back are not on it, for a measured reason: the line reached column 89 of 100 with the entries
// below, and either of them takes it past the width. Neither help line is truncated and the layout measures
// the footer by counting newlines, so anything that overflows either costs the list a row or is cut off the
// right edge. They are in the expanded help, in the column they belong to.
//
// The half page keys are not on it either, and that is the same space decision: navigation belongs in the
// expanded help beside the page and jump keys, which is where somebody looking for it looks.
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Attach, k.New, k.Kill, k.Rename, k.Preview, k.Help, k.Quit}
}

// FullHelp is what the help key expands to.
//
// The list's own navigation and filter keys are added by the model, which owns the list and so is the only
// thing that can ask it what its bindings are, and the half page keys go into the list's own navigation
// column rather than into one of these. See model.fullHelp.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Attach, k.Switch, k.New},
		{k.Kill, k.Rename},
		// Back sits with quit because that is what it is a spelling of.
		{k.Preview, k.Help, k.Quit, k.Back},
	}
}
