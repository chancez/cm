package client

import (
	"testing"
	"time"
)

// keys builds a set from written specs, failing the test on one that does not parse.
func keys(t *testing.T, specs ...string) keySet {
	t.Helper()
	set := make(keySet, 0, len(specs))
	for _, spec := range specs {
		key, err := ParseKeySpec(spec)
		if err != nil {
			t.Fatalf("ParseKeySpec(%q) error = %v", spec, err)
		}
		set = append(set, key)
	}
	return set
}

// Either key detaches, which is the point of a list: one for the keyboard at a desk and one for a layout
// where the first is awkward.
func TestEitherDetachKeyDetaches(t *testing.T) {
	g := &inputGate{detach: keys(t, `ctrl-\`, "ctrl-q")}
	for _, in := range []string{"\x1c", "\x11"} {
		got := g.feed([]byte(in), time.Now())
		if got.Action != gateDetach {
			t.Errorf("feed(%q) action = %v, want a detach", in, got.Action)
		}
	}
}

// The press that came first in the read wins, whichever list it is in, because that is the order the user
// typed. Taking the first list entry instead would reorder what they did.
func TestTheEarliestPressWins(t *testing.T) {
	g := &inputGate{detach: keys(t, "ctrl-q"), prefix: keys(t, "ctrl-]")}

	if got := g.feed([]byte("\x1d\x11"), time.Now()); got.Action != gatePrefix {
		t.Errorf("prefix then detach: action = %v, want the prefix", got.Action)
	}
	g = &inputGate{detach: keys(t, "ctrl-q"), prefix: keys(t, "ctrl-]")}
	if got := g.feed([]byte("\x11\x1d"), time.Now()); got.Action != gateDetach {
		t.Errorf("detach then prefix: action = %v, want the detach", got.Action)
	}
}

// A partial CSI encoding is held back for the longest key in either set. The shortest would forward the
// start of a longer encoding and miss that press, which is the bug the single-key holdback was written
// against and which a list makes easier to reintroduce.
func TestHoldBackCoversEveryKeyInTheSet(t *testing.T) {
	// The CSI forms carry the character's codepoint rather than its control code, so ctrl-\ is CSI 92;5u
	// and ctrl-q is CSI 113;5u. This tail could still become either.
	g := &inputGate{detach: keys(t, `ctrl-\`, "ctrl-q")}
	got := g.feed([]byte("ls\x1b[1"), time.Now())
	if string(got.Forward) != "ls" {
		t.Errorf("forwarded %q, want only the typing: the partial sequence must be held", got.Forward)
	}

	// And the rest of the sequence completes the press rather than reaching the session.
	got = g.feed([]byte("13;5u"), time.Now())
	if got.Action != gateDetach {
		t.Errorf("action = %v after the sequence completed, want a detach", got.Action)
	}
}

// An empty list is the default key rather than no key, which is what Options.readsTerminal asks: a
// read-only client whose detach key is unset still reads the terminal, and reading it as "off" stopped it
// from reading keys at all.
func TestAnEmptyKeySetIsNotADisabledOne(t *testing.T) {
	if keySet(nil).allDisabled() {
		t.Error("an empty set reads as disabled, so an unset detach_key would turn detaching off")
	}
	if !keys(t, "none").allDisabled() {
		t.Error(`a set of "none" does not read as disabled`)
	}
	// One live key among disabled ones is still live, since a set is matched as a whole.
	if keys(t, "none", "ctrl-q").allDisabled() {
		t.Error("a set with a live key reads as disabled")
	}
}

// The primary is the first live key, which is what the bar names and what ctrl-] q forwards: a help line
// listing three spellings of detach would spend its width on a fact nobody needs twice.
func TestThePrimaryIsTheFirstLiveKey(t *testing.T) {
	if got := keys(t, "none", "ctrl-q", `ctrl-\`).primary().Name; got != "ctrl-q" {
		t.Errorf("primary = %q, want ctrl-q", got)
	}
	if got := keySet(nil).primary(); !got.Disabled {
		t.Errorf("primary of an empty set = %+v, want a disabled spec", got)
	}
}

// The handover escape counts the action, not a spelling.
//
// Raised by another agent working on the handover, and it is the same insight as comparing chords by bytes
// rather than by name, one level up: with two detach keys bound, two presses of one and one of the other
// have to be three presses of "detach". A count that keyed on which spelling arrived would either never
// reach the escape or reach it at a moment the user did not intend, and the failure is a window closing
// with a live client still nested inside it.
func TestTheHandoverEscapeCountsTheActionNotTheKey(t *testing.T) {
	g := &inputGate{detach: keys(t, `ctrl-\`, "ctrl-q")}
	g.setNesting(true, 1)

	// Two presses, different spellings: the second is the one that warns.
	if got := g.feed([]byte("\x1c"), time.Now()); got.Action != gateNone {
		t.Fatalf("first press action = %v, want it forwarded silently", got.Action)
	}
	got := g.feed([]byte("\x11"), time.Now())
	if got.Action != gateNestedWarn {
		t.Fatalf("second press action = %v, want the warning", got.Action)
	}
	// And it names the key that was pressed, not the primary, since the notice asks for one more press.
	if got.Key.Name != "ctrl-q" {
		t.Errorf("the warning names %q, want ctrl-q, the key that arrived", got.Key.Name)
	}

	// A third, in the first spelling again, escapes the handover.
	if got := g.feed([]byte("\x1c"), time.Now()); got.Action != gateDetach {
		t.Errorf("third press action = %v, want the detach that escapes a handover nobody is acting on",
			got.Action)
	}
}
