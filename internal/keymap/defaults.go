package keymap

// DefaultDetachKey is ctrl-\, matching zmx.
//
// Here rather than in internal/client because it is a key like any other and this package is where keys
// are spelled; the client parses it into the form it matches bytes with.
const DefaultDetachKey = `ctrl-\`

// DefaultPrefixKey is ctrl-], which opens the overlay.
//
// Chosen against the alternatives on two counts. Ergonomics: left ctrl and a right-hand key, which ctrl-a
// and ctrl-b (screen and tmux) are not. And cost, which is what ruled the rest out: ctrl-o is vim's
// jumplist-back, ctrl-u ctrl-p ctrl-n ctrl-l are readline's, and every remaining right-hand control code
// is a key a program wants. ctrl-] costs vim's ctags tag-jump, which an LSP's gd has largely replaced.
// ctrl-space has the best ergonomics of all and was rejected for delivery: not every terminal sends NUL
// for it, and a key that silently does nothing on one machine is worse than a key that costs something
// everywhere.
const DefaultPrefixKey = "ctrl-]"

// Context is which of cm's interfaces a binding belongs to.
//
// Two, and they are kept apart rather than sharing action names, because the same verb has different
// keys in each today: the overlay kills with k under a program that is still drawing, the picker with x
// in a list where x is free. Merging them would have had to pick a winner and silently change one.
type Context string

const (
	// Overlay is the bar inside an attached session, including its session chooser.
	Overlay Context = "overlay"
	// TUI is `cm tui`, the full session picker.
	TUI Context = "tui"
)

// Action is one thing an interface can do.
//
// A string rather than an integer, because it is also the name in the config file: `[keys.overlay] next`
// is this constant. One spelling means a reader can go from a config file to the code that acts on it.
type Action string

// Overlay actions. The chooser's are in the same set as the bar's on purpose: they are modes of one
// interface, a key means one thing at a time, and a single list is what makes the collision check able
// to see a conflict between them.
const (
	OverlaySwitch     Action = "switch"
	OverlayNext       Action = "next"
	OverlayPrevious   Action = "previous"
	OverlayLast       Action = "last"
	OverlayName       Action = "name"
	OverlayKill       Action = "kill"
	OverlayPicker     Action = "picker"
	OverlayDetach     Action = "detach"
	OverlaySendDetach Action = "send-detach"
	OverlayCommand    Action = "command"
	OverlayHelp       Action = "help"

	// The chooser: a filtered list of sessions in a few rows.
	OverlayChoose      Action = "choose"
	OverlayUp          Action = "up"
	OverlayDown        Action = "down"
	OverlayErase       Action = "erase"
	OverlayClearFilter Action = "clear-filter"
)

// Picker actions, including the list's own navigation.
const (
	TUIAttach       Action = "attach"
	TUISwitch       Action = "switch"
	TUIBack         Action = "back"
	TUINew          Action = "new"
	TUIKill         Action = "kill"
	TUIRename       Action = "rename"
	TUIOutput       Action = "output"
	TUIHelp         Action = "help"
	TUIQuit         Action = "quit"
	TUIUp           Action = "up"
	TUIDown         Action = "down"
	TUIPageUp       Action = "page-up"
	TUIPageDown     Action = "page-down"
	TUIHalfPageUp   Action = "half-page-up"
	TUIHalfPageDown Action = "half-page-down"
	TUIStart        Action = "start"
	TUIEnd          Action = "end"
	TUIFilter       Action = "filter"
	TUIClearFilter  Action = "clear-filter"
)

// Definition is one action: what it is called, what it says in help, and which keys it starts with.
type Definition struct {
	Action Action
	// Label is the help text, short enough for a help line that is measured against the window.
	Label string
	// Defaults are the keys when the config says nothing, in the spelling a config file uses. Round
	// tripping through the same parser as a configured value is the point: a default that does not parse
	// is a failing test rather than a key nobody can press.
	Defaults []string
	// Always are keys this action keeps whatever the config says.
	//
	// One reason only: a way out that cannot be configured away. ctrl-c leaves the picker and escape
	// clears its filter, and a config that moved both elsewhere would leave a full-screen program with
	// no documented exit. Everything else is replaceable.
	Always []string
}

// definitions is the whole table, in the order actions are matched.
//
// Order is load-bearing twice. A keypress is looked up in this order, so when two actions in one context
// end up sharing a key the earlier one wins, deterministically rather than by map iteration. And help is
// rendered in this order, so the reading order of the bar and the help body is decided here rather than
// in each renderer.
var definitions = map[Context][]Definition{
	Overlay: {
		{Action: OverlaySwitch, Label: "switch session", Defaults: []string{"s"}},
		{Action: OverlayNext, Label: "next session", Defaults: []string{"n"}},
		{Action: OverlayPrevious, Label: "previous session", Defaults: []string{"p"}},
		{Action: OverlayLast, Label: "last visited session", Defaults: []string{"l"}},
		{Action: OverlayName, Label: "name this session", Defaults: []string{"b"}},
		{Action: OverlayKill, Label: "kill a session", Defaults: []string{"k"}},
		{Action: OverlayPicker, Label: "the full picker", Defaults: []string{"t"}},
		{Action: OverlayDetach, Label: "detach", Defaults: []string{"d"}},
		{Action: OverlaySendDetach, Label: "send the detach key to the program", Defaults: []string{"q"}},
		{Action: OverlayCommand, Label: "any cm command", Defaults: []string{":"}},
		{Action: OverlayHelp, Label: "help", Defaults: []string{"?"}},

		{Action: OverlayChoose, Label: "choose", Defaults: []string{"enter"}},
		// ctrl-j and ctrl-k are fzf's, ctrl-n and ctrl-p are readline's, and the arrows are what a hand
		// reaches for. Bare j and k cannot join them: every printable key filters the list, which is what
		// makes twenty sessions usable in six rows.
		{Action: OverlayUp, Label: "up", Defaults: []string{"ctrl-k", "ctrl-p", "up"}},
		{Action: OverlayDown, Label: "down", Defaults: []string{"ctrl-j", "ctrl-n", "down"}},
		{Action: OverlayErase, Label: "erase", Defaults: []string{"backspace"}},
		{Action: OverlayClearFilter, Label: "clear the filter", Defaults: []string{"ctrl-u"}},
	},
	TUI: {
		{Action: TUIAttach, Label: "attach", Defaults: []string{"enter"}},
		{Action: TUISwitch, Label: "switch here", Defaults: []string{"s"}},
		// No default: the way back is the prefix key of the client that opened this picker, which only
		// that client knows. See tui.Options.
		{Action: TUIBack, Label: "back to the session"},
		{Action: TUINew, Label: "new session", Defaults: []string{"n"}},
		{Action: TUIKill, Label: "kill", Defaults: []string{"x"}},
		{Action: TUIRename, Label: "rename", Defaults: []string{"r"}},
		{Action: TUIOutput, Label: "output", Defaults: []string{"p"}},
		{Action: TUIHelp, Label: "keys", Defaults: []string{"?"}},
		{Action: TUIQuit, Label: "quit", Defaults: []string{"q"}, Always: []string{"ctrl-c"}},
		{Action: TUIUp, Label: "up", Defaults: []string{"up", "k"}},
		{Action: TUIDown, Label: "down", Defaults: []string{"down", "j"}},
		{Action: TUIPageUp, Label: "prev page", Defaults: []string{"left", "h", "pageup", "b", "u"}},
		{Action: TUIPageDown, Label: "next page", Defaults: []string{"right", "l", "pagedown", "f", "d"}},
		// Half a page, as in vim and less, which is what the whole-page keys above leave missing.
		{Action: TUIHalfPageUp, Label: "half page up", Defaults: []string{"ctrl-u"}},
		{Action: TUIHalfPageDown, Label: "half page down", Defaults: []string{"ctrl-d"}},
		{Action: TUIStart, Label: "go to start", Defaults: []string{"home", "g"}},
		{Action: TUIEnd, Label: "go to end", Defaults: []string{"end", "G"}},
		{Action: TUIFilter, Label: "filter", Defaults: []string{"/"}},
		{Action: TUIClearFilter, Label: "clear filter", Always: []string{"escape"}},
	},
}

// Definitions returns a context's actions in the order they are matched and rendered.
func Definitions(ctx Context) []Definition { return definitions[ctx] }
