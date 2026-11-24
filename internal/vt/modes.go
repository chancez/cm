//go:build cgo

package vt

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"go.mitchellh.com/libghostty"

	"github.com/chancez/cm/internal/termmodes"
)

// recordedModes is what Modes reads: every mode ResetModes puts back except synchronized output, which a
// program sets and resets around every frame. gr toggles it per redraw, and nothing should survive a restart
// in the middle of one.
var recordedModes = slices.DeleteFunc(slices.Clone(resettableModes), func(m libghostty.Mode) bool {
	return m == libghostty.ModeSynchronizedOutput
})

// modeDefaults is a new terminal's value for each recorded mode, read once from one rather than written
// down, for the reason ResetModes reads its defaults: they cannot drift from libghostty's.
var modeDefaults = sync.OnceValues(func() (map[libghostty.Mode]bool, error) {
	fresh, err := New(1, 1, Callbacks{})
	if err != nil {
		return nil, err
	}
	defer fresh.Close()
	out := make(map[libghostty.Mode]bool, len(recordedModes))
	for _, m := range recordedModes {
		on, err := fresh.mode(m)
		if err != nil {
			return nil, err
		}
		out[m] = on
	}
	return out, nil
})

// modeKey spells a mode as its sequence does, which is the key termmodes records it under.
func modeKey(m libghostty.Mode) string {
	if m.ANSI() {
		return strconv.Itoa(int(m.Value()))
	}
	return "?" + strconv.Itoa(int(m.Value()))
}

// Modes returns every recorded mode whose value differs from a new terminal's, keyed as termmodes keys them:
// the modes themselves, the alternate screen, and the active screen's kitty keyboard flags.
//
// About 2.9us, measured by BenchmarkModes, which is why the server reads them only for a chunk
// termmodes.Watch says may have changed one.
func (t *Terminal) Modes() (map[string]int, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	defaults, err := modeDefaults()
	if err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, m := range recordedModes {
		on, err := t.mode(m)
		if err != nil {
			return nil, err
		}
		if on != defaults[m] {
			out[modeKey(m)] = boolInt(on)
		}
	}
	alt, err := t.OnAltScreen()
	if err != nil {
		return nil, err
	}
	if alt {
		out[termmodes.AltScreen] = 1
	}
	flags, err := t.kittyKeyboardFlags()
	if err != nil {
		return nil, err
	}
	if flags != 0 {
		out[termmodes.KittyKeyboard] = int(flags)
	}
	return out, nil
}

// ApplyModes sets modes as a program's own sequences would, for an adopting server whose replay lost the
// bytes that set them. See termmodes.Trimmed.
//
// Written as sequences rather than set through the API, so libghostty runs the handling a program's sequence
// gets, which is what ResetModes does in the other direction. The alternate screen goes first, since each
// screen has its own kitty keyboard stack and the flags belong to the screen the program is on; the flags go
// last, pushed, as a program pushes them, so the pop it sends on the way out finds them.
func (t *Terminal) ApplyModes(values map[string]int) error {
	if t.closed {
		return fmt.Errorf("terminal is closed")
	}
	var b strings.Builder
	if values[termmodes.AltScreen] != 0 {
		b.WriteString("\x1b[?1049h")
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		if k != termmodes.AltScreen && k != termmodes.KittyKeyboard {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !validModeKey(k) {
			return fmt.Errorf("applying modes: %q is not a mode", k)
		}
		final := "l"
		if values[k] != 0 {
			final = "h"
		}
		b.WriteString("\x1b[" + k + final)
	}
	if flags := values[termmodes.KittyKeyboard]; flags != 0 {
		fmt.Fprintf(&b, "\x1b[>%du", flags)
	}
	if b.Len() == 0 {
		return nil
	}
	return t.Write([]byte(b.String()))
}

// validModeKey reports whether k is digits, optionally after a "?", so a record read back from the store
// cannot write an arbitrary sequence into the model.
func validModeKey(k string) bool {
	k = strings.TrimPrefix(k, "?")
	if k == "" {
		return false
	}
	for _, c := range k {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
