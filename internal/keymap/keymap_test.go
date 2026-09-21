package keymap

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/input"
)

// Every default parses, every action says something in help, and no default collides with another in its
// own context.
//
// The guard that matters when an action is added: a new entry with a typo in its key, no label, or a key
// something else already uses fails here rather than in a terminal.
func TestDefaultsAreCompleteAndConsistent(t *testing.T) {
	for _, ctx := range []Context{Overlay, TUI} {
		m, problems := Build(ctx, nil)
		if len(problems) != 0 {
			t.Errorf("%s defaults have problems: %v", ctx, problems)
		}
		for _, def := range Definitions(ctx) {
			if def.Label == "" {
				t.Errorf("%s action %q has no help label", ctx, def.Action)
			}
			// TUIBack is the one action with no default: its key is the prefix key of whichever client
			// opened the picker, which only that client knows.
			if def.Action == TUIBack {
				if m.Bound(def.Action) {
					t.Errorf("%s: back has a default key, but only the caller knows it", ctx)
				}
				continue
			}
			if !m.Bound(def.Action) {
				t.Errorf("%s action %q is bound to nothing", ctx, def.Action)
			}
		}
	}
}

// A chord's name, the bytes a terminal sends for it, and bubbletea's spelling are three views of one
// keystroke, and this pairs them against the two sources rather than restating them.
//
// The failure it exists for is silent: a rename in bubbletea, or a change in internal/input's table,
// leaves a binding matching nothing, which looks exactly like a terminal that never sent the key.
func TestChordsAgreeWithTheKeyTables(t *testing.T) {
	for _, ctx := range []Context{Overlay, TUI} {
		m, _ := Build(ctx, nil)
		for _, def := range Definitions(ctx) {
			for _, c := range m.Chords(def.Action) {
				want, err := input.ParseKey(c.Name)
				if err != nil {
					t.Errorf("%s: internal/input does not know %q: %v", def.Action, c.Name, err)
					continue
				}
				if !reflect.DeepEqual(c.Bytes, want) {
					t.Errorf("%s %q sends %q, internal/input says %q", def.Action, c.Name, c.Bytes, want)
				}
			}
		}
	}
}

// The three shapes of chord, and what each matches.
func TestChordMatching(t *testing.T) {
	tests := []struct {
		spec  string
		press Press
		want  bool
	}{
		{spec: "s", press: Press{Rune: 's'}, want: true},
		{spec: "s", press: Press{Rune: 'S'}, want: false},
		{spec: "G", press: Press{Rune: 'G'}, want: true},
		{spec: "G", press: Press{Rune: 'g'}, want: false},
		{spec: "ctrl-n", press: Press{Ctrl: 'n'}, want: true},
		{spec: "ctrl-N", press: Press{Ctrl: 'n'}, want: true},
		{spec: "ctrl-n", press: Press{Rune: 'n'}, want: false},
		{spec: "enter", press: Press{Named: "enter"}, want: true},
		{spec: "return", press: Press{Named: "enter"}, want: true},
		{spec: "esc", press: Press{Named: "escape"}, want: true},
		{spec: "up", press: Press{Named: "down"}, want: false},
	}
	for _, tc := range tests {
		c, err := ParseChord(tc.spec)
		if err != nil {
			t.Errorf("ParseChord(%q) error = %v", tc.spec, err)
			continue
		}
		if got := c.Matches(tc.press); got != tc.want {
			t.Errorf("ParseChord(%q).Matches(%+v) = %v, want %v", tc.spec, tc.press, got, tc.want)
		}
	}
}

// One keystroke under two names is one keystroke. These three pairs share a byte, and a check on names
// rather than bytes would call them distinct: a config binding tab and ctrl-i to different actions would
// then look fine and one of them would never fire.
func TestChordsThatAreTheSameKeystroke(t *testing.T) {
	for _, pair := range [][2]string{
		{"tab", "ctrl-i"},
		{"enter", "ctrl-m"},
		{"escape", "ctrl-["},
		{"backspace", "ctrl-?"},
	} {
		a, err := ParseChord(pair[0])
		if err != nil {
			t.Fatalf("ParseChord(%q) error = %v", pair[0], err)
		}
		b, err := ParseChord(pair[1])
		if err != nil {
			t.Fatalf("ParseChord(%q) error = %v", pair[1], err)
		}
		if !a.Same(b) {
			t.Errorf("%q and %q are the same keystroke (%q and %q) but Same says otherwise",
				pair[0], pair[1], a.Bytes, b.Bytes)
		}
	}
}

// A configured list replaces the defaults, which is the only rule that can take a default away.
func TestConfiguredKeysReplaceTheDefaults(t *testing.T) {
	m, problems := Build(Overlay, map[string][]string{"next": {"j"}})
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if got, want := m.Names(OverlayNext), "j"; got != want {
		t.Errorf("next is bound to %q, want %q: a list replaces rather than adds", got, want)
	}
	if a, ok := m.Lookup(Press{Rune: 'n'}); ok {
		t.Errorf("n still means %q after next was moved to j", a)
	}
	if a, _ := m.Lookup(Press{Rune: 'j'}); a != OverlayNext {
		t.Errorf("j means %q, want next", a)
	}
}

// Several keys per action, which is the other half of the ask.
func TestAnActionTakesSeveralKeys(t *testing.T) {
	m, problems := Build(Overlay, map[string][]string{"last": {"l", "ctrl-o", "f5"}})
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if got, want := m.Names(OverlayLast), "l, ctrl-o, f5"; got != want {
		t.Errorf("last is bound to %q, want %q", got, want)
	}
	for _, p := range []Press{{Rune: 'l'}, {Ctrl: 'o'}, {Named: "f5"}} {
		if a, _ := m.Lookup(p); a != OverlayLast {
			t.Errorf("%+v means %q, want last", p, a)
		}
	}
}

// An empty list unbinds, which is what makes a key removable without giving it to something else.
func TestAnEmptyListUnbinds(t *testing.T) {
	m, problems := Build(Overlay, map[string][]string{"kill": {}})
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if m.Bound(OverlayKill) {
		t.Errorf("kill is still bound to %q", m.Names(OverlayKill))
	}
	if a, ok := m.Lookup(Press{Rune: 'k'}); ok {
		t.Errorf("k means %q, want nothing", a)
	}
}

// A key an action keeps whatever the config says: ctrl-c leaves the picker, escape clears its filter, and
// a config that moved both elsewhere would leave a full-screen program with no documented way out.
func TestSomeKeysCannotBeConfiguredAway(t *testing.T) {
	m, problems := Build(TUI, map[string][]string{"quit": {"Q"}, "clear-filter": {"ctrl-g"}})
	if len(problems) != 0 {
		t.Fatalf("problems = %v, want none", problems)
	}
	if got, want := m.Names(TUIQuit), "Q, ctrl-c"; got != want {
		t.Errorf("quit is bound to %q, want %q", got, want)
	}
	if got, want := m.Names(TUIClearFilter), "ctrl-g, escape"; got != want {
		t.Errorf("clear-filter is bound to %q, want %q", got, want)
	}
}

// Everything that can be wrong with a binding is reported rather than fatal, and cm carries on with what
// it understood. A config typo that refused every attach would take a person's terminal away.
func TestProblemsAreReportedAndSurvivable(t *testing.T) {
	m, problems := Build(Overlay, map[string][]string{
		"next":        {"nope-not-a-key", "j"},
		"previous":    {"alt-p"},
		"invented":    {"z"},
		"send-detach": {"s"},
	})

	var got []string
	for _, p := range problems {
		got = append(got, p.Setting+": "+p.Message)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{
		"keys.overlay.next: unknown key",
		"keys.overlay.previous: alt is not available",
		"keys.overlay.invented: unknown action",
		"is already bound to switch, which wins",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems do not mention %q:\n%s", want, joined)
		}
	}

	// And what was understood still works: the good key in a list with a bad one, and the action whose
	// key collided keeps its place behind the winner.
	if a, _ := m.Lookup(Press{Rune: 'j'}); a != OverlayNext {
		t.Errorf("j means %q, want next: the parseable key in the list must survive the unparseable one", a)
	}
	if a, _ := m.Lookup(Press{Rune: 's'}); a != OverlaySwitch {
		t.Errorf("s means %q, want switch: the earlier action wins a collision", a)
	}
	if m.Bound(OverlayPrevious) {
		t.Errorf("previous is bound to %q, want nothing: its only key was rejected", m.Names(OverlayPrevious))
	}
}

// A key bubbletea does not report cannot be a picker binding, and saying so beats a key that does nothing.
func TestAKeyThePickerCannotSeeIsReported(t *testing.T) {
	m, problems := Build(TUI, map[string][]string{"new": {"newline"}})
	if m.Bound(TUINew) {
		t.Errorf("new is bound to %q, want nothing", m.Names(TUINew))
	}
	var joined string
	for _, p := range problems {
		joined += p.String() + "\n"
	}
	if !strings.Contains(joined, "not a key bubbletea reports") {
		t.Errorf("problems do not explain why newline cannot be bound:\n%s", joined)
	}
}

// The caller's own key, which no default can carry: the picker's way back is the prefix key of whichever
// client opened it.
func TestACallerCanAddAKey(t *testing.T) {
	m, _ := Build(TUI, nil)
	m.Add(TUIBack, "ctrl-]")
	if got, want := m.Names(TUIBack), "ctrl-]"; got != want {
		t.Errorf("back is bound to %q, want %q", got, want)
	}
	// Twice is once, so a caller repeating itself does not double the help line.
	m.Add(TUIBack, "ctrl-]")
	if got, want := m.Names(TUIBack), "ctrl-]"; got != want {
		t.Errorf("back is bound to %q after being added twice, want %q", got, want)
	}
}
