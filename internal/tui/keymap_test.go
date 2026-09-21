package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/chancez/cm/internal/keymap"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// bound builds a picker whose keys came from a config table.
func bound(t *testing.T, bindings map[string][]string, sessions ...*serverv1.Session) *harness {
	t.Helper()
	keys, problems := keymap.Build(keymap.TUI, bindings)
	if len(problems) != 0 {
		t.Fatalf("bindings %v have problems: %v", bindings, problems)
	}
	return newHarnessWith(t, Options{Keys: keys}, sessions...)
}

// An action moved in the config is performed by the new key, and the default no longer does it.
func TestPickerActionsFollowTheConfiguredKeys(t *testing.T) {
	h := bound(t, map[string][]string{"kill": {"delete"}}, session("work", "a7k2m9x4", 100))
	h.list()

	h.press("x")
	if h.model.mode == modeConfirmKill {
		t.Error("x still offers to kill, but kill was moved to delete")
	}
	// bubbletea's own code for the key, since its name is what a binding matches: cm's "delete" and
	// bubbletea's are the same key under two byte spellings, and pressing 0x7f here would be backspace.
	h.pressCode(tea.KeyDelete)
	if h.model.mode != modeConfirmKill {
		t.Errorf("mode = %v after delete, want the kill confirmation", h.model.mode)
	}
}

// Several keys for one action.
func TestPickerTakesSeveralKeysForAnAction(t *testing.T) {
	for _, press := range []func(h *harness){
		func(h *harness) { h.press("p") },
		func(h *harness) { h.pressCtrl('v') },
	} {
		h := bound(t, map[string][]string{"output": {"p", "ctrl-v"}}, session("work", "a7k2m9x4", 100))
		h.list()
		was := h.model.preview.on
		press(h)
		if h.model.preview.on == was {
			t.Errorf("the output pane did not toggle, want both keys to work")
		}
	}
}

// The list's own navigation is configurable too, which is what stops half the keys in this window being
// cm's business and half being bubbles'.
func TestPickerListNavigationIsBindable(t *testing.T) {
	h := bound(t, map[string][]string{"down": {"ctrl-n"}, "up": {"ctrl-p"}},
		manySessions(5)...)
	h.list()

	h.pressCtrl('n')
	if got := h.model.list.Index(); got != 1 {
		t.Errorf("index after ctrl-n = %d, want 1", got)
	}
	h.pressCtrl('p')
	if got := h.model.list.Index(); got != 0 {
		t.Errorf("index after ctrl-p = %d, want 0", got)
	}
	// And j no longer moves, since a configured list replaces the default rather than adding to it.
	h.press("j")
	if got := h.model.list.Index(); got != 0 {
		t.Errorf("index after j = %d, want 0: the default was replaced", got)
	}
}

// An unbound action is absent from the help as well as inert, which is how an empty list reads as a
// decision rather than as a key cm lost.
func TestPickerAnUnboundActionIsAbsent(t *testing.T) {
	h := bound(t, map[string][]string{"rename": {}}, session("work", "a7k2m9x4", 100))
	h.list()

	h.press("r")
	if h.model.mode == modeRename {
		t.Error("r still renames, but rename was unbound")
	}
	h.press("?")
	if view := h.model.help.View(h.model.fullHelp()); strings.Contains(view, "rename") {
		t.Errorf("the help offers rename with no key:\n%s", view)
	}
}

// ctrl-c leaves whatever the config says, since a picker that answers no key is one people kill from
// another window.
func TestPickerCtrlCAlwaysQuits(t *testing.T) {
	h := bound(t, map[string][]string{"quit": {"Q"}}, session("work", "a7k2m9x4", 100))
	h.list()

	cmd := h.pressCtrl('c')
	if cmd == nil {
		t.Fatal("ctrl-c did nothing, want a quit")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("ctrl-c produced %T, want a quit", cmd())
	}
}
