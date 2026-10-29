//go:build cgo

// Package vt wraps libghostty-vt, through the go.mitchellh.com/libghostty binding, and is the only
// package in cm that touches it.
//
// Everything outside works with cm's own types: no binding types, no Ghostty handles, no result
// codes. That containment is deliberate: both libghostty's API and the binding's are unstable by
// their authors' own description, so a breaking change should mean editing one package.
//
// The binding owns the cgo. cm wrote its own until this package was moved onto it, and what that
// removed is the part that was easiest to get wrong: callback trampolines installed from C because a
// Go local's address cannot be handed to libghostty, function pointers passed as values where
// userdata is passed by address, and a process-wide C buffer for the XTVERSION reply.
//
// cgo is required. There was a stub standing in for this package without it, on the theory that cm should
// degrade rather than fail, and the degraded build was not worth having: `cm read`, `cm history`, and screen
// restore on reattach are most of what cm is for, and all three need the emulator. What the stub actually
// produced was a build where those commands returned empty *successfully*, which is a worse failure than not
// building -- it cost two debugging sessions, once when `cm run` printed nothing and once when a readiness
// check could never be satisfied, both times looking like a bug in cm rather than a missing emulator.
//
// libghostty provides parsing, screen and scrollback state with reflow, and formatting screen
// contents back out as escape sequences. It does not provide a pty, process spawning, or an
// event loop; cm implements those itself.
package vt

import (
	"errors"
	"fmt"
	"strings"

	"go.mitchellh.com/libghostty"
)

// Errors returned when libghostty rejects a call.
// Available reports whether the terminal emulator is compiled in.
//
// Always true now that cgo is required. Kept as a constant rather than deleted because `cm version` and
// `cm status` report it, and a client talking to a server built differently still wants to see the answer
// rather than assume it.
const Available = true

var (
	ErrOutOfMemory = errors.New("libghostty: out of memory")
	ErrInvalid     = errors.New("libghostty: invalid value")
	ErrNoValue     = errors.New("libghostty: no value")
)

// check translates a binding error into one of cm's, so callers outside this package match on
// these sentinels rather than on the binding's.
func check(err error, op string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, libghostty.ErrOutOfMemory):
		return fmt.Errorf("%s: %w", op, ErrOutOfMemory)
	case errors.Is(err, libghostty.ErrInvalidValue):
		return fmt.Errorf("%s: %w", op, ErrInvalid)
	case errors.Is(err, libghostty.ErrNoValue):
		return fmt.Errorf("%s: %w", op, ErrNoValue)
	default:
		return fmt.Errorf("%s: %w", op, err)
	}
}

// Callbacks receive terminal-initiated events.
//
// They fire synchronously inside Write, and libghostty forbids re-entering the terminal from
// them, so an implementation must only record or enqueue. In particular it must not call
// Write, Restore, or Resize on the same Terminal.
type Callbacks struct {
	// WritePty receives bytes the terminal generated that must reach the pty, such as
	// responses to device status reports. Wiring this is required for correctness rather
	// than a nicety: a program that queries the terminal and gets no answer hangs.
	//
	// The slice is only valid for the duration of the call.
	WritePty func(data []byte)
	// TitleChanged fires when the title changes via OSC 0 or OSC 2.
	TitleChanged func(title string)
	// PwdChanged fires when the shell reports its directory via OSC 7, OSC 9, or OSC 1337.
	// The value is whatever the shell emitted, unparsed, so it is usually a file:// URI.
	PwdChanged func(pwd string)
	// ReportXtversion enables answering XTVERSION (CSI > q) with the value SetXtversion recorded.
	//
	// A flag rather than the string itself, because the value is process-wide: see SetXtversion. A
	// per-terminal string here would read as though each terminal reports its own, which cm does not.
	ReportXtversion bool
}

// SetXtversion records what every terminal reports for XTVERSION (CSI > q), naming the terminal a
// program believes it is talking to.
//
// Process-wide, and kept that way for the callers rather than for the binding. The C side used to
// force it: libghostty requires the reply to outlive its callback, so it lived in one C buffer and a
// second terminal created with another value made both report the second. The binding's callback
// returns a string per terminal, so that limit is gone, but cm reports its own version from every
// session and has no use for per-terminal identity, so the API is unchanged.
//
// Left unset, libghostty answers "libghostty", which is true about the emulator and misleading about
// the terminal: a program asking what it is talking to gets the name of a library rather than the
// multiplexer holding its pty.
//
// Call before creating terminals. Not safe to call concurrently with New.
func SetXtversion(v string) {
	if v == "" {
		return
	}
	xtversion = v
}

// xtversion is what SetXtversion recorded.
var xtversion string

// Terminal is a terminal emulator: it consumes output and can render its screen back out as
// escape sequences.
//
// A Terminal is not safe for concurrent use. cm relies on that being cheap to guarantee,
// since exactly one goroutine feeds each session's output.
type Terminal struct {
	term   *libghostty.Terminal
	cb     Callbacks
	closed bool
}

// New creates a terminal of the given size.
func New(rows, cols uint16, cb Callbacks) (*Terminal, error) {
	if rows == 0 || cols == 0 {
		return nil, fmt.Errorf("terminal size %dx%d: %w", rows, cols, ErrInvalid)
	}

	// Installed here rather than by a caller, because every terminal wants it and forgetting is silent:
	// PNG transmissions are simply rejected and the model's screen quietly lacks the images on it. Once
	// per process, and libghostty's sys options are process-wide, so this is the right granularity.
	InstallPNGDecoder()

	t := &Terminal{cb: cb}
	opts := []libghostty.TerminalOption{
		libghostty.WithSize(cols, rows),
		// CSI 21 t, the title report, became opt-in in libghostty. On, because cm answered it before
		// and internal/query classifies it as answered: off, the query is stripped from every client
		// and nothing replies. TestQuerySetsAgreeWithEmulator caught the change as an OVERREACH.
		libghostty.WithTitleReport(true),
	}
	// Each callback is installed only when wanted, so a caller that does not want one does not pay for
	// it. The closures check closed for the same reason the C trampolines did: libghostty only calls
	// back during a write, so a callback after Close cannot normally happen, and a cheap guard is worth
	// it when the alternative would be a use-after-free.
	if cb.WritePty != nil {
		opts = append(opts, libghostty.WithWritePty(func(_ *libghostty.Terminal, data []byte) {
			if t.closed {
				return
			}
			// Copied: the buffer is only valid for this call, and the consumer will queue it.
			t.cb.WritePty(append([]byte(nil), data...))
		}))
	}
	if cb.TitleChanged != nil {
		opts = append(opts, libghostty.WithTitleChanged(func(*libghostty.Terminal) {
			if t.closed {
				return
			}
			if title, err := t.Title(); err == nil {
				t.cb.TitleChanged(title)
			}
		}))
	}
	if cb.PwdChanged != nil {
		opts = append(opts, libghostty.WithPwdChanged(func(*libghostty.Terminal) {
			if t.closed {
				return
			}
			if pwd, err := t.Pwd(); err == nil {
				t.cb.PwdChanged(pwd)
			}
		}))
	}
	if cb.ReportXtversion {
		// An empty reply makes libghostty report its own default, "libghostty", so an unset value
		// degrades to that rather than to an empty answer.
		opts = append(opts, libghostty.WithXtversion(func(*libghostty.Terminal) string { return xtversion }))
	}

	term, err := libghostty.NewTerminal(opts...)
	if err != nil {
		return nil, check(err, "creating terminal")
	}
	t.term = term
	return t, nil
}

// Write feeds terminal output to the emulator.
//
// Callbacks fire synchronously during this call.
func (t *Terminal) Write(p []byte) error {
	if t.closed {
		return errors.New("terminal is closed")
	}
	t.term.VTWrite(p)
	return nil
}

// Resize changes the terminal size, reflowing existing content.
//
// Reflow moves the cursor, which is why a screen snapshot must be taken before resizing
// rather than after.
//
// cellWidth and cellHeight are one cell in pixels, and they are what makes an image's position
// knowable: libghostty derives how many rows and columns a kitty graphics placement covers from them, so
// with zeros every placement reports itself off-screen and a restore has nothing to put back. Zero is
// still allowed and still means unknown, because a client that never reported a pixel size is a real
// case and text does not need the metrics.
func (t *Terminal) Resize(rows, cols, cellWidth, cellHeight uint16) error {
	if t.closed {
		return errors.New("terminal is closed")
	}
	return check(t.term.Resize(cols, rows, uint32(cellWidth), uint32(cellHeight)), "resizing terminal")
}

// SetScrollbackLimit bounds retained scrollback in lines.
//
// libghostty prunes at page granularity, so the effective limit is somewhat higher than
// requested. Zero means unlimited.
func (t *Terminal) SetScrollbackLimit(lines int) error {
	if lines <= 0 {
		return check(t.term.SetScrollbackMaxLines(nil), "clearing scrollback limit")
	}
	return check(t.term.SetScrollbackMaxLines(new(uint(lines))), "setting scrollback limit")
}

// Close releases the terminal.
//
// Safe to call more than once, because teardown can be reached both by an explicit close and
// by a failed constructor.
func (t *Terminal) Close() error {
	if t.closed {
		return nil
	}
	t.closed = true
	if t.term != nil {
		t.term.Close()
		t.term = nil
	}
	return nil
}

// Title returns the terminal's current title, or an empty string if unset.
func (t *Terminal) Title() (string, error) {
	title, err := t.term.Title()
	return title, check(err, "reading the title")
}

// Pwd returns the directory the shell last reported, unparsed, so usually a file:// URI.
//
// libghostty stores whatever bytes the shell emitted. Notably it appends a NUL sentinel that
// must not be forwarded to a client: kitty records it into its session file and then fails to
// parse its own state back. See zmx issue 222.
func (t *Terminal) Pwd() (string, error) {
	pwd, err := t.term.Pwd()
	if err != nil {
		return "", check(err, "reading the working directory")
	}
	// Trim the sentinel rather than relying on callers to know about it.
	return strings.TrimRight(pwd, "\x00"), nil
}

// cmSyncMode exposes the synchronized-output mode to tests in this package.
func cmSyncMode() libghostty.Mode { return libghostty.ModeSyncOutput }

// focusEventMode reports whether DECSET 1004 focus reporting is enabled.
func (t *Terminal) focusEventMode() (bool, error) {
	if t.closed {
		return false, nil
	}
	return t.mode(libghostty.ModeFocusEvent)
}

// kittyKeyboardFlags reports the kitty keyboard protocol flags in effect, 0 when the protocol is off.
//
// The active screen's flags, which is what the formatter reads too: libghostty keeps a separate stack
// per screen, so a program on the alternate screen has its own. Reading the active one is right because
// it is the screen the program is drawing on.
func (t *Terminal) kittyKeyboardFlags() (uint8, error) {
	if t.closed {
		return 0, nil
	}
	flags, err := t.term.KittyKeyboardFlags()
	if err != nil {
		return 0, check(err, "reading kitty keyboard flags")
	}
	return uint8(flags), nil
}

// InBandResizeMode reports whether the program has enabled mode 2048, in-band size reports.
//
// Read from the model rather than tracked separately because the model is already parsing every byte
// the program writes, so a second tracker would be a second thing to keep correct. The distinction that
// matters is between the program's request and cm's answer: cm answers DECRQM for this mode with "not
// recognized" (see DenyModes), and nvim sets it anyway without waiting for the reply. This reports what
// the program actually asked for, which is what decides whether a report is owed.
func (t *Terminal) InBandResizeMode() (bool, error) {
	if t.closed {
		return false, nil
	}
	return t.mode(libghostty.ModeInBandResize)
}
