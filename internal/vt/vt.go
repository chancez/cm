// Package vt wraps libghostty-vt, and is the only package in cm that uses cgo.
//
// Everything outside works with Go types: no Ghostty handles, no unsafe.Pointer, no result
// codes. That containment is deliberate: libghostty's API is unstable by upstream's own
// description, so a breaking change should mean editing one package.
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

/*
// These are the only #cgo directives in the package. They are package-scoped, so another file in
// package vt needing <ghostty/vt.h> just includes it; repeating the LDFLAGS line puts the same
// archive on the link line twice and ld warns about duplicate libraries on every build.
#cgo CFLAGS: -I${SRCDIR}/../../third_party/ghostty/zig-out/include
#cgo LDFLAGS: ${SRCDIR}/../../third_party/ghostty/zig-out/lib/libghostty-vt.a
#include <ghostty/vt.h>
#include <stdlib.h>
#include "callbacks.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime/cgo"
	"sync"
	"unsafe"
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

// check converts a GhosttyResult into an error.
func check(rc C.GhosttyResult, op string) error {
	switch rc {
	case C.GHOSTTY_SUCCESS:
		return nil
	case C.GHOSTTY_OUT_OF_MEMORY:
		return fmt.Errorf("%s: %w", op, ErrOutOfMemory)
	case C.GHOSTTY_INVALID_VALUE:
		return fmt.Errorf("%s: %w", op, ErrInvalid)
	case C.GHOSTTY_NO_VALUE:
		return fmt.Errorf("%s: %w", op, ErrNoValue)
	default:
		return fmt.Errorf("%s: libghostty error %d", op, int(rc))
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
	// per-terminal string here would read as though each terminal could report its own, which is not
	// what the C side does.
	ReportXtversion bool
}

// SetXtversion records what every terminal reports for XTVERSION (CSI > q), naming the terminal a
// program believes it is talking to.
//
// Process-wide, and deliberately shaped to say so. libghostty requires the reply to stay valid until
// its callback returns, so the value lives in a C buffer rather than in Go memory, and there is one
// buffer: a terminal created with one value and then another created with a different one makes
// *both* report the second. Verified rather than assumed, and it is why this is a package function
// instead of a field on Callbacks.
//
// That costs nothing here, since cm reports its own version and every session agrees on it. It would
// matter to anything wanting per-session identity, which would need a per-terminal buffer keyed on
// the handle.
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
	cs := C.CString(v)
	// Freed immediately: cm_set_xtversion copies into its own buffer, so this allocation only has to
	// survive the call.
	defer C.free(unsafe.Pointer(cs))
	C.cm_set_xtversion(cs)
}

// Terminal is a terminal emulator: it consumes output and can render its screen back out as
// escape sequences.
//
// A Terminal is not safe for concurrent use. cm relies on that being cheap to guarantee,
// since exactly one goroutine feeds each session's output.
type Terminal struct {
	ptr C.GhosttyTerminal
	// handle keeps the Go side reachable from C without passing a Go pointer, which cgo
	// forbids for anything stored across a call boundary.
	handle cgo.Handle
	cb     Callbacks
	closed bool
}

// New creates a terminal of the given size.
func New(rows, cols uint16, cb Callbacks) (*Terminal, error) {
	if rows == 0 || cols == 0 {
		return nil, fmt.Errorf("terminal size %dx%d: %w", rows, cols, ErrInvalid)
	}

	t := &Terminal{cb: cb}
	if err := check(C.ghostty_terminal_new(nil, &t.ptr, C.uint16_t(cols), C.uint16_t(rows)),
		"creating terminal"); err != nil {
		return nil, err
	}

	t.handle = cgo.NewHandle(t)
	if err := t.wireCallbacks(); err != nil {
		t.Close()
		return nil, err
	}
	return t, nil
}

// wireCallbacks installs the trampolines libghostty should call.
//
// Done entirely in C. ghostty_terminal_set takes a pointer to the value being set, so doing this
// from Go would mean handing libghostty the address of a Go local: cgo forbids passing such a
// pointer, and it crashes with SIGBUS once that stack slot is reused.
func (t *Terminal) wireCallbacks() error {
	return check(C.cm_install_callbacks(
		t.ptr,
		C.uintptr_t(t.handle),
		C.bool(t.cb.WritePty != nil),
		C.bool(t.cb.TitleChanged != nil),
		C.bool(t.cb.PwdChanged != nil),
		C.bool(t.cb.ReportXtversion),
	), "installing callbacks")
}

// Write feeds terminal output to the emulator.
//
// Callbacks fire synchronously during this call.
func (t *Terminal) Write(p []byte) error {
	if t.closed {
		return errors.New("terminal is closed")
	}
	if len(p) == 0 {
		return nil
	}
	C.ghostty_terminal_vt_write(t.ptr, (*C.uint8_t)(unsafe.Pointer(&p[0])), C.size_t(len(p)))
	return nil
}

// Resize changes the terminal size, reflowing existing content.
//
// Reflow moves the cursor, which is why a screen snapshot must be taken before resizing
// rather than after.
func (t *Terminal) Resize(rows, cols uint16) error {
	if t.closed {
		return errors.New("terminal is closed")
	}
	return check(
		C.ghostty_terminal_resize(t.ptr, C.uint16_t(cols), C.uint16_t(rows), 0, 0),
		"resizing terminal",
	)
}

// SetScrollbackLimit bounds retained scrollback in lines.
//
// libghostty prunes at page granularity, so the effective limit is somewhat higher than
// requested. Zero means unlimited.
func (t *Terminal) SetScrollbackLimit(lines int) error {
	if lines <= 0 {
		return check(
			C.ghostty_terminal_set(t.ptr, C.GHOSTTY_TERMINAL_OPT_SCROLLBACK_MAX_LINES, nil),
			"clearing scrollback limit",
		)
	}
	n := C.size_t(lines)
	// Safe to pass the address of a local here: libghostty reads the value during the call and
	// does not retain the pointer, unlike the callback options.
	return check(
		C.ghostty_terminal_set(t.ptr, C.GHOSTTY_TERMINAL_OPT_SCROLLBACK_MAX_LINES,
			unsafe.Pointer(&n)),
		"setting scrollback limit",
	)
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
	if t.ptr != nil {
		C.ghostty_terminal_free(t.ptr)
		t.ptr = nil
	}
	if t.handle != 0 {
		t.handle.Delete()
		t.handle = 0
	}
	return nil
}

// stringData reads a string-valued field from the terminal.
func (t *Terminal) stringData(field C.GhosttyTerminalData) (string, error) {
	var s C.GhosttyString
	rc := C.ghostty_terminal_get(t.ptr, field, unsafe.Pointer(&s))
	if rc == C.GHOSTTY_NO_VALUE {
		return "", nil
	}
	if err := check(rc, "reading terminal data"); err != nil {
		return "", err
	}
	if s.ptr == nil || s.len == 0 {
		return "", nil
	}
	return C.GoStringN((*C.char)(unsafe.Pointer(s.ptr)), C.int(s.len)), nil
}

// Title returns the terminal's current title, or an empty string if unset.
func (t *Terminal) Title() (string, error) {
	return t.stringData(C.GHOSTTY_TERMINAL_DATA_TITLE)
}

// Pwd returns the directory the shell last reported, unparsed, so usually a file:// URI.
//
// libghostty stores whatever bytes the shell emitted. Notably it appends a NUL sentinel that
// must not be forwarded to a client: kitty records it into its session file and then fails to
// parse its own state back. See zmx issue 222.
func (t *Terminal) Pwd() (string, error) {
	pwd, err := t.stringData(C.GHOSTTY_TERMINAL_DATA_PWD)
	if err != nil {
		return "", err
	}
	// Trim the sentinel rather than relying on callers to know about it.
	for len(pwd) > 0 && pwd[len(pwd)-1] == 0 {
		pwd = pwd[:len(pwd)-1]
	}
	return pwd, nil
}

// callbackRegistry guards against a callback firing on a terminal that is being torn down.
//
// libghostty only calls back during Write, so this cannot normally happen, but a stale handle
// would be a use-after-free rather than an error, which is worth a cheap guard.
var callbackRegistry sync.Mutex

// terminalFromHandle recovers the Go terminal for a callback.
//
// userdata is the cgo handle itself, passed as an integer through C, so this converts it back
// rather than dereferencing anything.
func terminalFromHandle(userdata unsafe.Pointer) *Terminal {
	if userdata == nil {
		return nil
	}
	h := cgo.Handle(uintptr(userdata))
	t, _ := h.Value().(*Terminal)
	return t
}

//export cmWritePty
func cmWritePty(_ C.GhosttyTerminal, userdata unsafe.Pointer, data *C.uint8_t, length C.size_t) {
	callbackRegistry.Lock()
	defer callbackRegistry.Unlock()

	t := terminalFromHandle(userdata)
	if t == nil || t.cb.WritePty == nil || t.closed {
		return
	}
	// Copy: the buffer is only valid for this call, and the consumer will queue it.
	t.cb.WritePty(C.GoBytes(unsafe.Pointer(data), C.int(length)))
}

//export cmTitleChanged
func cmTitleChanged(_ C.GhosttyTerminal, userdata unsafe.Pointer) {
	callbackRegistry.Lock()
	defer callbackRegistry.Unlock()

	t := terminalFromHandle(userdata)
	if t == nil || t.cb.TitleChanged == nil || t.closed {
		return
	}
	title, err := t.Title()
	if err != nil {
		return
	}
	t.cb.TitleChanged(title)
}

//export cmPwdChanged
func cmPwdChanged(_ C.GhosttyTerminal, userdata unsafe.Pointer) {
	callbackRegistry.Lock()
	defer callbackRegistry.Unlock()

	t := terminalFromHandle(userdata)
	if t == nil || t.cb.PwdChanged == nil || t.closed {
		return
	}
	pwd, err := t.Pwd()
	if err != nil {
		return
	}
	t.cb.PwdChanged(pwd)
}

// cmSyncMode exposes the synchronized-output mode to tests in this package, which cannot
// reference the C accessor from a non-cgo file.
func cmSyncMode() C.GhosttyMode { return C.cm_mode_sync_output() }

// focusEventMode reports whether DECSET 1004 focus reporting is enabled.
func (t *Terminal) focusEventMode() (bool, error) {
	if t.closed {
		return false, nil
	}
	return t.mode(C.cm_mode_focus_event())
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
	var flags C.uint8_t
	if err := check(C.ghostty_terminal_get(t.ptr, C.GHOSTTY_TERMINAL_DATA_KITTY_KEYBOARD_FLAGS,
		unsafe.Pointer(&flags)), "reading kitty keyboard flags"); err != nil {
		return 0, err
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
	return t.mode(C.cm_mode_in_band_resize())
}
