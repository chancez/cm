package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
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

// teaKey translates cm's spelling into bubbletea's, and this pairs the two against bubbletea itself. A
// rename upstream would otherwise leave the binding matching nothing, which looks exactly like a key the
// terminal never sent.
func TestTeaKeyMatchesBubbletea(t *testing.T) {
	for _, tc := range []struct {
		spec string
		code rune
	}{
		{spec: "ctrl-]", code: ']'},
		{spec: `ctrl-\`, code: '\\'},
		{spec: "ctrl-o", code: 'o'},
		// cm names this key after the byte it resolves to, NUL, and bubbletea names the same press
		// "ctrl+space". Worth pinning: it is the one spec whose character is not in its name.
		{spec: "ctrl-space", code: ' '},
	} {
		got := teaKey(tc.spec)
		want := tea.KeyPressMsg{Code: tc.code, Mod: tea.ModCtrl}.String()
		if got != want {
			t.Errorf("teaKey(%q) = %q, want %q, which is what bubbletea calls that press", tc.spec, got, want)
		}
	}

	// Not a ctrl- combination, so there is no key to bind. "none" is the reachable case: a prefix key the
	// user turned off.
	for _, spec := range []string{"", "none", "ctrl-", "f1"} {
		if got := teaKey(spec); got != "" {
			t.Errorf("teaKey(%q) = %q, want no key", spec, got)
		}
	}
}
