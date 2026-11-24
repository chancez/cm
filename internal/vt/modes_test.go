//go:build cgo

package vt

import (
	"maps"
	"reflect"
	"testing"

	"go.mitchellh.com/libghostty"

	"github.com/chancez/cm/internal/termmodes"
)

// grStartup is what gr writes on startup, captured from a kitty --dump-bytes of a real run.
const grStartup = "\x1b[?1049h\x1b[?25l\x1b[?2004h\x1b[?1003h\x1b[?1006h\x1b[>1u\x1b[2J\x1b[?2026h\x1b[?2027hgr"

func TestModesReadsWhatAProgramSet(t *testing.T) {
	term := newTerminal(t, 24, 80)
	if err := term.Write([]byte(grStartup)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := term.Modes()
	if err != nil {
		t.Fatalf("Modes() error = %v", err)
	}
	// ?2026 is still set, since gr has not finished the frame, and is left out: it is reset around every
	// frame.
	want := map[string]int{
		termmodes.AltScreen:     1,
		"?25":                   0,
		"?2004":                 1,
		"?1003":                 1,
		"?1006":                 1,
		"?2027":                 1,
		termmodes.KittyKeyboard: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Modes() = %v, want %v", got, want)
	}
}

func TestModesOfANewTerminalIsEmpty(t *testing.T) {
	term := newTerminal(t, 24, 80)
	got, err := term.Modes()
	if err != nil {
		t.Fatalf("Modes() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Modes() = %v, want none", got)
	}
}

// Applying what a program set reproduces it: the same modes, and a restore a client can scroll in. The
// restore is the part that broke, since it is what an attaching kitty is put into.
func TestApplyModesReproducesWhatAProgramSet(t *testing.T) {
	program := newTerminal(t, 24, 80)
	if err := program.Write([]byte(grStartup)); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	want, err := program.Modes()
	if err != nil {
		t.Fatalf("Modes() error = %v", err)
	}

	applied := newTerminal(t, 24, 80)
	if err := applied.ApplyModes(maps.Clone(want)); err != nil {
		t.Fatalf("ApplyModes() error = %v", err)
	}
	got, err := applied.Modes()
	if err != nil {
		t.Fatalf("Modes() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Modes() after ApplyModes = %v, want %v", got, want)
	}
	for _, m := range []libghostty.Mode{libghostty.ModeMouseEventAny, libghostty.ModeMouseFormatSGR} {
		on, err := applied.mode(m)
		if err != nil || !on {
			t.Fatalf("mode %d = %v, %v; want set", m.Value(), on, err)
		}
	}
}

func TestApplyModesRefusesAKeyThatIsNotAMode(t *testing.T) {
	term := newTerminal(t, 24, 80)
	if err := term.ApplyModes(map[string]int{"?1049h\x1b]2;owned\x07\x1b[?1": 1}); err == nil {
		t.Fatal("ApplyModes() wrote a key that is not a mode")
	}
}

// What reading the modes costs, which is paid for every chunk termmodes.Watch cannot rule out.
func BenchmarkModes(b *testing.B) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		b.Fatal(err)
	}
	defer term.Close()
	if err := term.Write([]byte(grStartup)); err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if _, err := term.Modes(); err != nil {
			b.Fatal(err)
		}
	}
}
