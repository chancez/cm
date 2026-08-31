package tui

import "charm.land/bubbles/v2/key"

// keyMap is the picker's own bindings, separate from the list's navigation and filter keys.
//
// A bubbles key.Map rather than a switch on strings, because the help line is then generated from the
// same bindings it describes: a key that moves and a help text that does not is a documentation bug
// that a reader blames on the tool.
type keyMap struct {
	Attach  key.Binding
	New     key.Binding
	Kill    key.Binding
	Rename  key.Binding
	Preview key.Binding
	Help    key.Binding
	Quit    key.Binding
}

func defaultKeys() keyMap {
	return keyMap{
		Attach: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "attach"),
		),
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
func (k keyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Attach, k.New, k.Kill, k.Rename, k.Preview, k.Help, k.Quit}
}

// FullHelp is what "?" expands to.
//
// The list's own navigation and filter keys are added by the model, which owns the list and so is the
// only thing that can ask it what its bindings are. See model.fullHelp.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{{k.Attach, k.New}, {k.Kill, k.Rename}, {k.Preview, k.Help, k.Quit}}
}
