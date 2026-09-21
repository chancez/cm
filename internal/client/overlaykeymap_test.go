package client

import (
	"strings"
	"testing"
)

// An action moved in the config is performed by the new key and not by the old one.
func TestOverlayActionsFollowTheConfiguredKeys(t *testing.T) {
	o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{"detach": {"x"}})
	o.open()

	if got := o.feed([]byte("d")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("d = %+v, want nothing: detach was moved to x", got)
	}
	o, _ = newTestOverlayWithKeys(t, 24, 80, map[string][]string{"detach": {"x"}})
	o.open()
	if got := o.feed([]byte("x")); !sameResponse(got, overlayResponse{Detach: true, Repaint: true}) {
		t.Errorf("x = %+v, want a detach", got)
	}
}

// Several keys for one action, which is the other half of what a keymap is for. A control key and a
// character reach the overlay by different paths in the decoder, so both are pressed here.
func TestOverlayTakesSeveralKeysForAnAction(t *testing.T) {
	for _, key := range [][]byte{[]byte("t"), {0x0f}} {
		o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{"picker": {"t", "ctrl-o"}})
		o.canPick = true
		o.open()
		if got := o.feed(key); !sameResponse(got, overlayResponse{OpenPicker: true, Repaint: true}) {
			t.Errorf("feed(%q) = %+v, want the picker", key, got)
		}
	}
}

// An action with an empty list has no key, and the key it used to have says so rather than acting.
func TestOverlayAnUnboundActionDoesNothing(t *testing.T) {
	o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{"kill": {}})
	o.open()

	o.feed([]byte("k"))
	if o.mode != overlayResult || !strings.Contains(o.status, "no action") {
		t.Errorf("mode %v status %q, want a message saying k does nothing", o.mode, o.status)
	}
}

// A key the decoder used to fold into a meaning is now bindable. ctrl-g reached the overlay as "a control
// byte nothing binds" and was dropped, so no config could have used it.
func TestOverlayCanBindAControlKeyItUsedToDrop(t *testing.T) {
	o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{"detach": {"ctrl-g"}})
	o.open()

	if got := o.feed([]byte{0x07}); !sameResponse(got, overlayResponse{Detach: true, Repaint: true}) {
		t.Errorf("ctrl-g = %+v, want a detach", got)
	}
}

// The chooser's movement keys are bindings too, so a user who wants the arrows or j and k can have them.
//
// Bare j and k are the interesting case: every printable key filters the list, so binding one to movement
// takes it out of the filter. That is the user's trade to make, and the defaults are control keys because
// it is not one cm should make for them.
func TestOverlayChooserMovementIsBindable(t *testing.T) {
	o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{
		"down": {"j"},
		"up":   {"k"},
	})
	o.open()
	o.feed([]byte("s"))
	o.sessions(pickItems(3), nil)

	o.feed([]byte("j"))
	if got := o.pick.cursor; got != 1 {
		t.Errorf("cursor after j = %d, want 1", got)
	}
	if got := string(o.pick.filter); got != "" {
		t.Errorf("filter = %q, want empty: j was bound to movement", got)
	}
	o.feed([]byte("k"))
	if got := o.pick.cursor; got != 0 {
		t.Errorf("cursor after k = %d, want 0", got)
	}
	// And ctrl-j no longer moves, since the list replaced the defaults rather than adding to them.
	o.feed([]byte{0x0a})
	if got := o.pick.cursor; got != 0 {
		t.Errorf("cursor after ctrl-j = %d, want 0: the default was replaced", got)
	}
}

// The help and the bar name the configured keys, not the defaults.
//
// The failure this prevents is the one AGENTS.md calls a documentation bug that a reader blames on the
// tool: a rebinding that leaves the help describing keys nobody has.
func TestOverlayHelpAndBarNameTheConfiguredKeys(t *testing.T) {
	o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{
		"switch": {"w"},
		"help":   {"h"},
	})
	o.open()

	rows := o.rows(24, 80)
	if len(rows) == 0 || !strings.Contains(rows[0], "w switch") {
		t.Errorf("bar = %q, want it to name w for switch", rows)
	}
	if strings.Contains(rows[0], "s switch") {
		t.Errorf("bar = %q, still names the default key", rows)
	}

	o.feed([]byte("h"))
	body := strings.Join(o.rows(24, 80), "\n")
	if !strings.Contains(body, "w  switch session") {
		t.Errorf("help = %q, want it to name w for switch", body)
	}
	if !strings.Contains(body, "h  help") {
		t.Errorf("help = %q, want it to name h for help", body)
	}
}

// An unbound action is left out of the help rather than listed with a blank key, since an empty list is a
// decision rather than a key cm lost.
func TestOverlayHelpLeavesOutAnUnboundAction(t *testing.T) {
	o, _ := newTestOverlayWithKeys(t, 24, 80, map[string][]string{"command": {}})
	o.open()
	o.feed([]byte("?"))

	body := strings.Join(o.rows(24, 80), "\n")
	if strings.Contains(body, "any cm command") {
		t.Errorf("help = %q, want no entry for the unbound command action", body)
	}
	if !strings.Contains(body, "switch session") {
		t.Errorf("help = %q, want the actions that are bound", body)
	}
}
