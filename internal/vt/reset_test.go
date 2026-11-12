//go:build cgo

package vt

import (
	"maps"
	"slices"
	"testing"

	"go.mitchellh.com/libghostty"
)

// modeTable reads every resettable mode, plus the kitty keyboard flags, which are not a mode but are what
// left a revived session unusable.
func modeTable(t *testing.T, term *Terminal) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, m := range resettableModes {
		on, err := term.mode(m)
		if err != nil {
			t.Fatalf("mode(%d) error = %v", m.Value(), err)
		}
		out[modeSequence(m, true)] = on
	}
	flags, err := term.kittyKeyboardFlags()
	if err != nil {
		t.Fatalf("kittyKeyboardFlags() error = %v", err)
	}
	out["kitty keyboard"] = flags != 0
	return out
}

// Every mode a program can leave set goes back to a new terminal's value, and the content and cursor stay
// where they were. Each mode is flipped from its default rather than a chosen few set, so a mode the
// reset misses fails here instead of in a revived session.
func TestResetModesRestoresEveryDefault(t *testing.T) {
	for _, alt := range []bool{false, true} {
		name := "main screen"
		if alt {
			name = "alternate screen"
		}
		t.Run(name, func(t *testing.T) {
			const content = "$ prog\r\nkept"
			term := newTerminal(t, 24, 80)
			if err := term.Write([]byte(content)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if alt {
				if err := term.Write([]byte("\x1b[?1049h\x1b[>3uNVIM")); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
			}
			if err := term.Write([]byte("\x1b[>5u\x1b7\x1b[3;20r\x1b8\x1b(0\x1b[1;31m\x1b[5 q")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			for _, m := range resettableModes {
				on, err := term.mode(m)
				if err != nil {
					t.Fatalf("mode() error = %v", err)
				}
				// Through the API, since DECOM as a sequence would home the cursor and the cursor is
				// part of what is asserted.
				if err := term.setMode(m, !on); err != nil {
					t.Fatalf("setMode() error = %v", err)
				}
			}

			if err := term.ResetModes(); err != nil {
				t.Fatalf("ResetModes() error = %v", err)
			}

			clean := newTerminal(t, 24, 80)
			if err := clean.Write([]byte(content)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if got, want := modeTable(t, term), modeTable(t, clean); !maps.Equal(got, want) {
				t.Errorf("modes after reset = %v, want %v", got, want)
			}
			got, err := term.Restore()
			if err != nil {
				t.Fatalf("Restore() error = %v", err)
			}
			want, err := clean.Restore()
			if err != nil {
				t.Fatalf("Restore() error = %v", err)
			}
			if string(got) != string(want) {
				t.Errorf("restore after reset =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// modeSequence spells both kinds of mode, since an ANSI mode written as a DEC private one sets a
// different mode entirely.
func TestModeSequence(t *testing.T) {
	got := []string{
		modeSequence(libghostty.ModeInsert, true),
		modeSequence(libghostty.ModeInsert, false),
		modeSequence(libghostty.ModeFocusEvent, true),
		modeSequence(libghostty.ModeFocusEvent, false),
	}
	want := []string{"\x1b[4h", "\x1b[4l", "\x1b[?1004h", "\x1b[?1004l"}
	if !slices.Equal(got, want) {
		t.Errorf("modeSequence() = %q, want %q", got, want)
	}
}
