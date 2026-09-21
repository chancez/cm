package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/chancez/cm/internal/keymap"
)

// The caller's prefix key leaves the picker, which is what makes ctrl-] t and ctrl-] a round trip.
func TestTheBackKeyLeavesThePicker(t *testing.T) {
	h := newHarnessWith(t, Options{BackKey: "ctrl-]"}, session("work", "a7k2m9x4", 100))
	h.list()

	cmd := h.pressCtrl(']')
	if cmd == nil {
		t.Fatal("the back key did nothing, want the picker to quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("the back key produced %T, want a quit", cmd())
	}
}

// A picker nobody is waiting on offers no such key: absent, not inert. Run from a shell the outer client
// intercepts its own prefix, so the key never arrives here, and a binding for it would describe a key that
// does nothing.
func TestTheBackKeyIsAbsentWithoutACaller(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	if h.model.keys.Back.Enabled() {
		t.Error("the back binding is enabled with nobody to go back to")
	}
	if cmd := h.pressCtrl(']'); cmd != nil {
		t.Errorf("the back key produced %T, want nothing: the binding is disabled", cmd())
	}

	h.press("?")
	if got := h.model.help.View(h.model.fullHelp()); strings.Contains(got, "back to the session") {
		t.Errorf("the help offers a way back with nobody to go back to:\n%s", got)
	}
}

// The key named is the caller's, not ctrl-], since prefix_key is configurable and only the client knows
// what it actually intercepts.
func TestTheBackKeyIsTheCallersKey(t *testing.T) {
	h := newHarnessWith(t, Options{BackKey: "ctrl-o"}, session("work", "a7k2m9x4", 100))
	h.list()

	if cmd := h.pressCtrl(']'); cmd != nil {
		t.Errorf("ctrl-] produced %T, want nothing: the caller configured ctrl-o", cmd())
	}
	cmd := h.pressCtrl('o')
	if cmd == nil {
		t.Fatal("ctrl-o did nothing, want the picker to quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("ctrl-o produced %T, want a quit", cmd())
	}

	h.press("?")
	if got := h.model.help.View(h.model.fullHelp()); !strings.Contains(got, "ctrl-o") {
		t.Errorf("the help names a key the caller did not configure:\n%s", got)
	}
}

// A filter being typed keeps every printable key, and the back key is a control key, so it still leaves.
// The reverse of the trap the action keys had: bindings checked before the filter made typing a name run
// commands.
func TestTheBackKeyWorksWhileFiltering(t *testing.T) {
	h := newHarnessWith(t, Options{BackKey: "ctrl-]"}, session("work", "a7k2m9x4", 100))
	h.list()
	h.press("/")
	h.press("w")

	cmd := h.pressCtrl(']')
	if cmd == nil {
		t.Fatal("the back key did nothing while filtering, want the picker to quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("the back key produced %T while filtering, want a quit", cmd())
	}
}

// Every key cm can bind is paired against what bubbletea calls that press.
//
// The two have their own names for the same keystroke, and internal/keymap carries both spellings because
// neither package can be asked at the point of use. This is where the pairing is checked, since this is the
// package that imports bubbletea. The failure it exists for is silent: a rename upstream leaves a binding
// matching nothing, which looks exactly like a terminal that never sent the key.
func TestChordNamesMatchBubbletea(t *testing.T) {
	for _, tc := range []struct {
		spec string
		msg  tea.KeyPressMsg
	}{
		{spec: "enter", msg: tea.KeyPressMsg{Code: tea.KeyEnter}},
		{spec: "escape", msg: tea.KeyPressMsg{Code: tea.KeyEscape}},
		{spec: "tab", msg: tea.KeyPressMsg{Code: tea.KeyTab}},
		{spec: "space", msg: tea.KeyPressMsg{Code: tea.KeySpace}},
		{spec: "backspace", msg: tea.KeyPressMsg{Code: tea.KeyBackspace}},
		{spec: "delete", msg: tea.KeyPressMsg{Code: tea.KeyDelete}},
		{spec: "insert", msg: tea.KeyPressMsg{Code: tea.KeyInsert}},
		{spec: "up", msg: tea.KeyPressMsg{Code: tea.KeyUp}},
		{spec: "down", msg: tea.KeyPressMsg{Code: tea.KeyDown}},
		{spec: "left", msg: tea.KeyPressMsg{Code: tea.KeyLeft}},
		{spec: "right", msg: tea.KeyPressMsg{Code: tea.KeyRight}},
		{spec: "home", msg: tea.KeyPressMsg{Code: tea.KeyHome}},
		{spec: "end", msg: tea.KeyPressMsg{Code: tea.KeyEnd}},
		// cm spells these in full and bubbletea abbreviates them, which is exactly the kind of difference
		// that would otherwise be found by a key that stopped working.
		{spec: "pageup", msg: tea.KeyPressMsg{Code: tea.KeyPgUp}},
		{spec: "pagedown", msg: tea.KeyPressMsg{Code: tea.KeyPgDown}},
		{spec: "f1", msg: tea.KeyPressMsg{Code: tea.KeyF1}},
		{spec: "f12", msg: tea.KeyPressMsg{Code: tea.KeyF12}},
		{spec: "ctrl-]", msg: tea.KeyPressMsg{Code: ']', Mod: tea.ModCtrl}},
		{spec: `ctrl-\`, msg: tea.KeyPressMsg{Code: '\\', Mod: tea.ModCtrl}},
		{spec: "ctrl-u", msg: tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}},
		// NUL, which cm names after the key rather than after the byte.
		{spec: "ctrl-space", msg: tea.KeyPressMsg{Code: tea.KeySpace, Mod: tea.ModCtrl}},
		{spec: "/", msg: tea.KeyPressMsg{Code: '/', Text: "/"}},
		{spec: "G", msg: tea.KeyPressMsg{Code: 'G', Text: "G"}},
	} {
		chord, err := keymap.ParseChord(tc.spec)
		if err != nil {
			t.Errorf("ParseChord(%q) error = %v", tc.spec, err)
			continue
		}
		if got, want := chord.Tea, tc.msg.String(); got != want {
			t.Errorf("keymap calls %q %q, bubbletea calls that press %q", tc.spec, got, want)
		}
	}
}

// And every default binding in the picker is a key bubbletea reports, so no default can be one this
// window could never receive.
func TestEveryPickerDefaultIsAKeyBubbleteaReports(t *testing.T) {
	m, problems := keymap.Build(keymap.TUI, nil)
	if len(problems) != 0 {
		t.Fatalf("defaults have problems: %v", problems)
	}
	for _, def := range m.Actions() {
		for _, c := range m.Chords(def.Action) {
			if c.Tea == "" {
				t.Errorf("%s is bound to %q, which bubbletea does not report", def.Action, c.Name)
			}
		}
	}
}
