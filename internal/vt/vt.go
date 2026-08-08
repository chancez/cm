// Package vt wraps libghostty-vt, and is the only package in cm that uses cgo.
//
// Everything outside works with Go types: no Ghostty handles, no unsafe.Pointer, no result
// codes. That containment is deliberate. libghostty's API is unstable by upstream's own
// description, so a breaking change should mean editing one package, and the other layers
// stay buildable with CGO_ENABLED=0, which matters because the shim needs no terminal
// emulation at all.
//
// libghostty provides parsing, screen and scrollback state with reflow, and formatting screen
// contents back out as escape sequences. It does not provide a pty, process spawning, or an
// event loop; cm implements those itself.
package vt

/*
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
	// userdata is the C-allocated box holding handle, kept so it can be freed.
	userdata unsafe.Pointer
	cb       Callbacks
	closed   bool
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
func (t *Terminal) wireCallbacks() error {
	// The handle is stored in C-allocated memory rather than converted straight to a pointer.
	// A cgo.Handle is an integer, and casting an integer to unsafe.Pointer produces something
	// that looks like a pointer to nothing, which the garbage collector and go vet both object
	// to. Boxing it keeps the value valid for as long as libghostty holds it.
	t.userdata = C.malloc(C.size_t(unsafe.Sizeof(C.uintptr_t(0))))
	if t.userdata == nil {
		return ErrOutOfMemory
	}
	*(*C.uintptr_t)(t.userdata) = C.uintptr_t(t.handle)

	if err := t.set(C.GHOSTTY_TERMINAL_OPT_USERDATA, t.userdata); err != nil {
		return err
	}

	if t.cb.WritePty != nil {
		fn := C.cm_write_pty_fn()
		if err := t.set(C.GHOSTTY_TERMINAL_OPT_WRITE_PTY, unsafe.Pointer(&fn)); err != nil {
			return err
		}
	}
	if t.cb.TitleChanged != nil {
		fn := C.cm_title_changed_fn()
		if err := t.set(C.GHOSTTY_TERMINAL_OPT_TITLE_CHANGED, unsafe.Pointer(&fn)); err != nil {
			return err
		}
	}
	if t.cb.PwdChanged != nil {
		fn := C.cm_pwd_changed_fn()
		if err := t.set(C.GHOSTTY_TERMINAL_OPT_PWD_CHANGED, unsafe.Pointer(&fn)); err != nil {
			return err
		}
	}
	return nil
}

func (t *Terminal) set(opt C.GhosttyTerminalOption, val unsafe.Pointer) error {
	return check(C.ghostty_terminal_set(t.ptr, opt, val),
		fmt.Sprintf("setting terminal option %d", int(opt)))
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
	return t.set(C.GHOSTTY_TERMINAL_OPT_SCROLLBACK_MAX_LINES, unsafe.Pointer(&n))
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
	if t.userdata != nil {
		C.free(t.userdata)
		t.userdata = nil
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

// terminalFromHandle recovers the Go terminal for a callback by unboxing the handle libghostty
// was given as userdata.
func terminalFromHandle(userdata unsafe.Pointer) *Terminal {
	if userdata == nil {
		return nil
	}
	h := cgo.Handle(*(*C.uintptr_t)(userdata))
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
