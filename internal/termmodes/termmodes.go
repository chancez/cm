// Package termmodes records the terminal modes a program in a session has set, and where in its output it
// set each one.
//
// A mode is set once, usually at a program's startup, and holds until something changes it. The server's
// terminal model learns it by parsing that byte, and a restarting server rebuilds its model by replaying only
// what the shim still retains, 4 MiB. A full-screen program that has drawn more than that since it started
// has had its setup trimmed away, so the rebuilt model is on the main screen with no mouse reporting, and
// every client attaching afterwards is told so. Measured on a gr that had run for three days: ?1049h and
// ?1003h at byte 11435653, an adoption whose oldest retained byte was about 12.4M, and from then on a window
// where keys worked and the wheel scrolled kitty's scrollback.
//
// So the modes are kept as state of their own, each with the position of the chunk that set it. An adopting
// server applies the ones set before the oldest retained byte, which the replay cannot set, and leaves the
// rest to the replay, which sets them where they really changed. Applying the state after the replay instead
// would be wrong for the alternate screen: the program's frames would already be in the main screen's
// scrollback, and switching afterwards clears what the alternate screen should hold.
package termmodes

import (
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/chancez/cm/internal/seq"
)

// AltScreen is the key the alternate screen is recorded under, whichever of 47, 1047 or 1049 entered it.
// 1049 is what an apply writes, since it is what full-screen programs use.
const AltScreen = "?1049"

// KittyKeyboard is the key the active screen's kitty keyboard flags are recorded under. Its value is the flags
// rather than on or off.
const KittyKeyboard = "kitty_keyboard"

// Mode is one mode whose value differs from a new terminal's, and the chunk that set it.
//
// Only values that differ are kept, which is what makes the apply rule exact: a mode back at its default
// needs nothing, since the model an adopting server builds starts at the defaults.
type Mode struct {
	// Key is the mode as its sequence spells it: "?1003" for a DEC private mode, "4" for an ANSI one, or
	// KittyKeyboard.
	Key string `json:"key"`
	// Value is 1 for set and 0 for reset, or the flags for KittyKeyboard.
	Value int `json:"value"`
	// SetAt is where the chunk that last changed this mode starts, in the shim's numbering, since that is
	// what an adopting server compares it with: the shim's oldest retained byte.
	SetAt seq.Shim `json:"set_at"`
}

// Tracker keeps each recorded mode's SetAt across observations, so an unchanged mode keeps the position
// that set it however many chunks follow.
type Tracker struct {
	modes map[string]Mode
}

// NewTracker starts from modes already recorded, which is what adoption hands over.
func NewTracker(seed []Mode) *Tracker {
	t := &Tracker{modes: make(map[string]Mode, len(seed))}
	for _, m := range seed {
		t.modes[m.Key] = m
	}
	return t
}

// Observe records the model's modes after the chunk starting at at, and reports whether anything changed.
//
// values is every mode that differs from a new terminal's. A mode missing from it is back at its default
// and is dropped.
func (t *Tracker) Observe(values map[string]int, at seq.Shim) bool {
	changed := false
	for k, v := range values {
		if m, ok := t.modes[k]; ok && m.Value == v {
			continue
		}
		t.modes[k] = Mode{Key: k, Value: v, SetAt: at}
		changed = true
	}
	for k := range t.modes {
		if _, ok := values[k]; !ok {
			delete(t.modes, k)
			changed = true
		}
	}
	return changed
}

// Modes returns what is recorded, sorted by key so equal states compare and print equal.
func (t *Tracker) Modes() []Mode {
	out := slices.Collect(maps.Values(t.modes))
	Sort(out)
	return out
}

// Sort orders modes by key.
func Sort(modes []Mode) {
	slices.SortFunc(modes, func(a, b Mode) int { return strings.Compare(a.Key, b.Key) })
}

// Trimmed returns the recorded modes set before oldest, the first byte a replay will feed the model.
//
// Those are the ones the replay cannot set, because the bytes that set them are gone. The rest are set by
// the replay itself, at their own position, and applying them early would put whatever came before them in
// the wrong state too.
//
// SetAt is a chunk's start rather than the setting byte's own position, so a chunk the trim cut through
// counts as trimmed. That errs toward applying: a mode applied and then set again by the replay costs at most
// one chunk of output drawn in its final state, where a mode not applied is the bug this package exists for.
func Trimmed(recorded []Mode, oldest seq.Shim) []Mode {
	var out []Mode
	for _, m := range recorded {
		if m.SetAt < oldest {
			out = append(out, m)
		}
	}
	return out
}

// Values returns modes as the key to value map Tracker.Observe and an apply take.
func Values(modes []Mode) map[string]int {
	out := make(map[string]int, len(modes))
	for _, m := range modes {
		out[m.Key] = m.Value
	}
	return out
}

// Format renders modes for a person, as the sequences that set them: "?1049h ?1003h ?25l kitty_keyboard=1".
func Format(modes []Mode) string {
	parts := make([]string, 0, len(modes))
	for _, m := range modes {
		switch {
		case m.Key == KittyKeyboard:
			parts = append(parts, KittyKeyboard+"="+strconv.Itoa(m.Value))
		case m.Value != 0:
			parts = append(parts, m.Key+"h")
		default:
			parts = append(parts, m.Key+"l")
		}
	}
	return strings.Join(parts, " ")
}

// Watch says whether a chunk of output can have changed a mode, so the model is read only for those.
//
// Reading every mode is about forty calls into libghostty, and most chunks are text and cursor motion. What
// can change one: a DEC private mode, CSI ? ... h or l; an ANSI one, CSI digits h or l; the kitty keyboard
// stack, CSI > u, CSI < u and CSI = u; and the two resets, RIS and DECSTR. A sequence split across two
// chunks is caught by carrying the unfinished escape into the next call.
type Watch struct {
	carry []byte
}

// maxCarry bounds what is held from an unfinished escape: past it the sequence is not one that sets a mode,
// which is a handful of digits.
const maxCarry = 16

// MayChange reports whether p, read after everything earlier passed to this Watch, may have changed a mode.
func (w *Watch) MayChange(p []byte) bool {
	data := p
	if len(w.carry) > 0 {
		data = append(w.carry, p...)
		w.carry = nil
	}
	found := false
	for i := 0; i < len(data); i++ {
		if data[i] != 0x1b {
			continue
		}
		hit, done := escapeSetsMode(data[i+1:])
		if hit {
			found = true
		}
		if !done && len(data)-i <= maxCarry {
			w.carry = append([]byte(nil), data[i:]...)
			break
		}
	}
	return found
}

// escapeSetsMode reads the bytes after an ESC and reports whether they begin a sequence that sets a mode,
// and whether they were enough to decide.
func escapeSetsMode(p []byte) (hit, done bool) {
	if len(p) == 0 {
		return false, false
	}
	switch p[0] {
	case 'c':
		return true, true
	case '[':
	default:
		return false, true
	}
	if len(p) == 1 {
		return false, false
	}
	switch p[1] {
	case '?', '>', '<', '=', '!':
		return true, true
	}
	for _, c := range p[1:] {
		switch {
		case c >= '0' && c <= '9', c == ';':
			continue
		case c == 'h', c == 'l':
			return true, true
		default:
			return false, true
		}
	}
	return false, false
}
