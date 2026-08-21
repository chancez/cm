//go:build cgo

package vt

/*
#include <ghostty/vt.h>
#include <stdlib.h>
#include "callbacks.h"
*/
import "C"

import (
	"bytes"
	"fmt"
	"unsafe"
)

// Restore returns bytes that reproduce the current screen when written to a fresh terminal.
//
// This is a port of zmx's serializeTerminalState, and every deviation from the obvious
// approach below is a bug someone already hit. See zmx/src/util.zig.
//
// Two phases, because one does not work. Scrollback is emitted first as bare content so it
// scrolls into the client's own scrollback; then the visible screen is cleared; then the
// viewport is emitted with terminal state. A single pass that emits everything with a cursor
// position leaves the cursor wrong, since the scrollback lines shift where the viewport
// starts. (zmx issue 31.)
func (t *Terminal) Restore() ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}

	// Synchronized output (DECSET 2026) is a handshake between a program and the terminal
	// currently showing it. Replaying it to a new client leaves that client withholding
	// renders until its own timeout fires, so it is suppressed across serialization and
	// restored afterwards.
	syncMode := C.cm_mode_sync_output()
	hadSync, err := t.mode(syncMode)
	if err == nil && hadSync {
		_ = t.setMode(syncMode, false)
		defer t.setMode(syncMode, true)
	}

	var buf bytes.Buffer

	// Say which screen this content belongs to, rather than leaving it to the default.
	//
	// The formatter emits only modes that differ from libghostty's defaults, and the main screen *is*
	// the default, so a main-screen blob carries no ?1049l at all. That is silently wrong for a client
	// whose terminal is on the alternate screen: the repaint lands on the alternate screen, over the
	// program's own display, and the real main screen underneath still holds whatever was there. The
	// next ?1049l any program sends then pops the terminal back to that stale screen and discards
	// everything cm painted.
	//
	// The symptom was quitting nvim and being left at a prompt with the whole nvim window still above
	// it. It reads as cm failing to clear the screen, and the screen it failed to leave is one cm never
	// knew the client was on.
	//
	// The two sides are not symmetric, which is why only this direction needs writing out. A blob for a
	// session *on* the alternate screen already opens with ?1049h from the mode state, so it says where
	// it belongs; a main-screen blob had no way to say so.
	//
	// Unconditional rather than only when the model was ever on the alternate screen. cm cannot see the
	// client's terminal, so "did this session use the alternate screen" is the wrong question: a client
	// can be left there by a program that ran before cm was in the path, and after a server restart the
	// model does not remember either way. Safe to send always, measured both ways rather than assumed:
	// against libghostty the contents are byte-identical with and without it, and in a real kitty all
	// 60 lines of scrollback survive it with the visible screen unchanged.
	//
	// The clear belongs *after* the switch, which is the part that is easy to get wrong. A client
	// clears its terminal before writing this blob, so that clear lands on whichever screen the
	// terminal is currently on: the alternate one gets wiped, the switch to main follows, and main
	// still holds its stale contents. Measured that way, the restored screen came back as
	// "STALE_REAL_MAIN\nSHELL_HISTORY\nPROMPT_LINE" with the stale line on top. Clearing here mirrors
	// the other direction rather than adding anything, since ?1049h is defined as save cursor, switch,
	// *and clear*. That ?1049l does not clear is the asymmetry that produced the bug.
	//
	// Held aside rather than written now, so it stays in front of the content and out of the empty
	// check below. See the prepend at the end.
	screenPrefix := ""
	if onAlt, err := t.mode(C.cm_mode_alt_screen_save()); err == nil && !onAlt {
		screenPrefix = "\x1b[?1049l\x1b[2J\x1b[H"
	}

	hasScrollback, err := t.emitScrollback(&buf)
	if err != nil {
		return nil, err
	}
	if hasScrollback {
		// Push the visible rows into scrollback rather than erasing them, then home the cursor
		// and reset SGR so styles from scrollback do not bleed into the viewport.
		//
		// No trailing newline is needed first. The formatter puts \r\n between lines but not
		// after the last one, so the cursor is still on that line; scrolling moves it up along
		// with everything else, whereas adding a newline as well would leave a blank row at the
		// boundary.
		//
		// The obvious choice here is ED (\x1b[2J), and it is wrong. ED erases the visible rows
		// in place, and the rows still visible at this point are the *tail of the scrollback*
		// just emitted, so erasing them destroys real content. It only looks correct when the
		// terminal is tall relative to the scrollback being restored, which is why a small
		// terminal loses lines and a large one appears fine.
		//
		// SU (\x1b[<n>S) scrolls instead, moving those rows up into scrollback where they
		// belong and leaving a blank viewport for the next phase.
		rows, _, err := t.Size()
		if err != nil {
			return nil, err
		}
		fmt.Fprintf(&buf, "\x1b[%dS\x1b[H\x1b[0m", rows)
	}

	if err := t.emitViewport(&buf); err != nil {
		return nil, err
	}

	// The formatter has no title extra and never emits OSC 0 or 2, so without this a
	// reattaching client shows whatever its terminal defaults to, usually the client's own
	// process name. OSC 2 does not move the cursor, so appending it after content is safe.
	// (zmx issue 224.)
	if title, err := t.Title(); err == nil && title != "" {
		fmt.Fprintf(&buf, "\x1b]2;%s\x07", title)
	}

	// Emitted by hand rather than via the formatter's pwd extra, which includes a NUL
	// sentinel that kitty persists into its session file and then cannot parse back.
	// (zmx issue 222.)
	//
	// Terminated with BEL rather than ST (ESC backslash), which is the more common form and the
	// one that works. Verified in a real kitty: with an ST terminator here, the *next* OSC in
	// the stream is swallowed and the sequence after that loses its ESC, so a cursor move like
	// ESC[179C renders as the literal text "179C" next to the prompt. The same test with BEL is
	// clean. Since the value is a URI it cannot contain a BEL, so there is nothing to escape.
	if pwd, err := t.Pwd(); err == nil && pwd != "" {
		fmt.Fprintf(&buf, "\x1b]7;%s\x07", pwd)
	}

	if buf.Len() == 0 {
		return nil, nil
	}
	// Prepended rather than written first, so an empty screen still returns nothing at all. See
	// screenPrefix above.
	return append([]byte(screenPrefix), buf.Bytes()...), nil
}

// emitScrollback writes scrollback content, reporting whether there was any.
//
// Content only, no modes or cursor: these lines are meant to scroll past the viewport into
// the client's own scrollback, and emitting state with them would apply it at the wrong time.
func (t *Terminal) emitScrollback(buf *bytes.Buffer) (bool, error) {
	screenTop, ok, err := t.gridRef(C.GHOSTTY_POINT_TAG_SCREEN, 0, 0)
	if err != nil || !ok {
		return false, err
	}
	activeTop, ok, err := t.gridRef(C.GHOSTTY_POINT_TAG_ACTIVE, 0, 0)
	if err != nil || !ok {
		return false, err
	}

	// Equal refs mean the active area starts at the top of the screen, so there is no
	// scrollback to emit.
	if screenTop == activeTop {
		return false, nil
	}

	// Scrollback ends on the row above the active area, and on that row's *last* column.
	//
	// Both details matter. A linear selection ending at column 0 covers only that single cell,
	// so the rest of the final row would be dropped. And ending at activeTop rather than the
	// row above it would duplicate a row that the viewport phase emits again.
	activeY, ok, err := t.screenY(activeTop)
	if err != nil || !ok || activeY == 0 {
		return false, err
	}
	_, cols, err := t.Size()
	if err != nil {
		return false, err
	}

	sbEnd, ok, err := t.gridRef(C.GHOSTTY_POINT_TAG_SCREEN, cols-1, activeY-1)
	if err != nil || !ok {
		return false, err
	}

	sel := C.GhosttySelection{}
	sel.size = C.size_t(unsafe.Sizeof(sel))
	sel.start = screenTop
	sel.end = sbEnd
	sel.rectangle = C.bool(false)

	out, err := t.format(formatOptions{
		emit:      C.GHOSTTY_FORMATTER_FORMAT_VT,
		selection: &sel,
	})
	if err != nil {
		return false, err
	}
	buf.Write(out)
	return true, nil
}

// emitViewport writes the visible screen along with the terminal state needed to reproduce it.
func (t *Terminal) emitViewport(buf *bytes.Buffer) error {
	opts := formatOptions{
		emit: C.GHOSTTY_FORMATTER_FORMAT_VT,
		// Modes, the scrolling region, and keyboard state are what make a restored screen
		// behave rather than merely look right.
		modes:           true,
		scrollingRegion: true,
		keyboard:        true,
		cursor:          true,
		style:           true,
		hyperlink:       true,
		// Tabstop restoration emits sequences that move the cursor after it has been
		// positioned, which corrupts where the shell thinks it is.
		tabstops: false,
		// The palette is the client terminal's own business, and replaying it would override
		// the user's theme.
		palette: false,
		// Emitted by hand in Restore, without the NUL sentinel this would include.
		pwd: false,
	}

	// Restrict to the active area. Without a selection the formatter emits scrollback too,
	// which phase one already covered.
	//
	// The bottom-right corner is resolved by walking up from the last row rather than assuming
	// row rows-1 exists. After output ending in a newline the cursor sits on a row that has no
	// cells yet, and a reference to it does not resolve, which would silently drop a real row
	// of content from this phase.
	topLeft, okTL, err := t.gridRef(C.GHOSTTY_POINT_TAG_ACTIVE, 0, 0)
	if err != nil {
		return err
	}
	rows, cols, err := t.Size()
	if err != nil {
		return err
	}
	var (
		bottomRight C.GhosttyGridRef
		okBR        bool
	)
	for y := int(rows) - 1; y >= 0; y-- {
		ref, ok, err := t.gridRef(C.GHOSTTY_POINT_TAG_ACTIVE, cols-1, uint32(y))
		if err != nil {
			return err
		}
		if ok {
			bottomRight, okBR = ref, true
			break
		}
	}

	var sel C.GhosttySelection
	if okTL && okBR {
		sel = C.GhosttySelection{}
		sel.size = C.size_t(unsafe.Sizeof(sel))
		sel.start = topLeft
		sel.end = bottomRight
		sel.rectangle = C.bool(false)
		opts.selection = &sel
	}
	// If either ref could not be resolved the whole screen is emitted instead. Slightly wrong
	// is better than empty.

	out, err := t.format(opts)
	if err != nil {
		return err
	}
	buf.Write(out)
	return nil
}

// Plain returns the terminal's contents as plain text, for a history dump.
func (t *Terminal) Plain() ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	return t.format(formatOptions{emit: C.GHOSTTY_FORMATTER_FORMAT_PLAIN})
}

// Tail returns the last lines of the terminal's contents as plain text.
//
// Separate from Plain because a caller reading a session programmatically wants a bounded, parseable
// view rather than the whole scrollback: a build log is megabytes, and the failure is in the last screen
// of it.
//
// unwrap rejoins soft-wrapped lines, which is what makes the result parseable. A line the terminal broke
// to fit its width is one line as far as the program that wrote it is concerned, and splitting it puts a
// newline into the middle of a path or a stack frame. The formatter does this natively, so there is no
// heuristic here about which breaks were soft.
//
// A lines value of zero means everything, so a caller can ask for the whole thing without a second code
// path.
func (t *Terminal) Tail(lines int, unwrap bool) ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	out, err := t.format(formatOptions{
		emit:   C.GHOSTTY_FORMATTER_FORMAT_PLAIN,
		unwrap: unwrap,
		// Trailing whitespace is padding the terminal added to fill a row, not content. Keeping it makes
		// every line look ragged to a caller matching on it.
		trim: true,
	})
	if err != nil || lines <= 0 {
		return out, err
	}
	return lastLines(out, lines), nil
}

// lastLines returns the final n lines of p.
//
// Counted from the end rather than by splitting the whole buffer, since the buffer can be megabytes and
// only its tail is wanted.
func lastLines(p []byte, n int) []byte {
	if n <= 0 || len(p) == 0 {
		return p
	}
	// A trailing newline ends the last line rather than starting an empty one, so it is not counted.
	end := len(p)
	if p[end-1] == '\n' {
		end--
	}
	count := 0
	for i := end - 1; i >= 0; i-- {
		if p[i] != '\n' {
			continue
		}
		count++
		if count == n {
			return p[i+1:]
		}
	}
	return p
}

// TailVT returns the last lines of the terminal's contents as escape sequences.
//
// The raw counterpart of Tail, for a caller that wants what a program actually emitted rather than the text it
// rendered to. VT covers the whole scrollback; this bounds it, which is what makes `cm read --raw --lines N`
// possible where `cm history --format vt` can only dump everything.
//
// Counting lines in escaped output is approximate by nature, since a line's worth of bytes includes whatever
// styling it carries. lastLines counts newlines, so a run of styling attached to the line before a boundary
// stays with that line. The alternative is reconstructing the sequence state at an arbitrary offset, which
// would mean re-emitting the attributes in force at the cut and is more machinery than a bounded raw dump
// justifies.
func (t *Terminal) TailVT(lines int) ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	out, err := t.format(formatOptions{
		emit:  C.GHOSTTY_FORMATTER_FORMAT_VT,
		style: true,
	})
	if err != nil || lines <= 0 {
		return out, err
	}
	return lastLines(out, lines), nil
}

// HTML returns the terminal's contents as HTML, preserving styling.
func (t *Terminal) HTML() ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	return t.format(formatOptions{emit: C.GHOSTTY_FORMATTER_FORMAT_HTML, style: true})
}

// VT returns the terminal's full contents, scrollback included, as escape sequences.
func (t *Terminal) VT() ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	return t.format(formatOptions{
		emit:     C.GHOSTTY_FORMATTER_FORMAT_VT,
		modes:    true,
		keyboard: true,
		cursor:   true,
		style:    true,
	})
}

// formatOptions mirrors the subset of the formatter's options cm uses, so callers above do not
// deal with C structs.
type formatOptions struct {
	emit            C.GhosttyFormatterFormat
	unwrap          bool
	trim            bool
	selection       *C.GhosttySelection
	modes           bool
	scrollingRegion bool
	keyboard        bool
	tabstops        bool
	palette         bool
	pwd             bool
	cursor          bool
	style           bool
	hyperlink       bool
}

// format runs the formatter and returns its output.
func (t *Terminal) format(o formatOptions) ([]byte, error) {
	opts := C.GhosttyFormatterTerminalOptions{}
	opts.size = C.size_t(unsafe.Sizeof(opts))
	opts.emit = o.emit
	opts.unwrap = C.bool(o.unwrap)
	opts.trim = C.bool(o.trim)
	opts.selection = o.selection
	opts.extra.modes = C.bool(o.modes)
	opts.extra.scrolling_region = C.bool(o.scrollingRegion)
	opts.extra.keyboard = C.bool(o.keyboard)
	opts.extra.tabstops = C.bool(o.tabstops)
	opts.extra.palette = C.bool(o.palette)
	opts.extra.pwd = C.bool(o.pwd)
	opts.extra.screen.cursor = C.bool(o.cursor)
	opts.extra.screen.style = C.bool(o.style)
	opts.extra.screen.hyperlink = C.bool(o.hyperlink)

	var f C.GhosttyFormatter
	if err := check(C.ghostty_formatter_terminal_new(nil, &f, t.ptr, opts),
		"creating formatter"); err != nil {
		return nil, err
	}
	defer C.ghostty_formatter_free(f)

	var (
		out    *C.uint8_t
		outLen C.size_t
	)
	if err := check(C.ghostty_formatter_format_alloc(f, nil, &out, &outLen),
		"formatting terminal"); err != nil {
		return nil, err
	}
	if out == nil || outLen == 0 {
		return nil, nil
	}
	// Copy into Go memory before releasing the C allocation.
	defer C.free(unsafe.Pointer(out))
	return C.GoBytes(unsafe.Pointer(out), C.int(outLen)), nil
}

// gridRef resolves a point to a grid reference, reporting whether it exists.
//
// An out-of-bounds point is not an error: an empty or short screen legitimately has no cell at
// a given position, and callers fall back rather than fail.
func (t *Terminal) gridRef(tag C.GhosttyPointTag, x uint16, y uint32) (C.GhosttyGridRef, bool, error) {
	var pt C.GhosttyPoint
	pt.tag = tag
	coord := (*C.GhosttyPointCoordinate)(unsafe.Pointer(&pt.value))
	coord.x = C.uint16_t(x)
	coord.y = C.uint32_t(y)

	var ref C.GhosttyGridRef
	rc := C.ghostty_terminal_grid_ref(t.ptr, pt, &ref)
	switch rc {
	case C.GHOSTTY_SUCCESS:
		return ref, true, nil
	case C.GHOSTTY_INVALID_VALUE, C.GHOSTTY_NO_VALUE:
		return ref, false, nil
	default:
		return ref, false, check(rc, "resolving grid reference")
	}
}

// Size reports the terminal's dimensions.
func (t *Terminal) Size() (rows, cols uint16, err error) {
	var v C.uint16_t
	if err := check(C.ghostty_terminal_get(t.ptr, C.GHOSTTY_TERMINAL_DATA_ROWS,
		unsafe.Pointer(&v)), "reading rows"); err != nil {
		return 0, 0, err
	}
	rows = uint16(v)
	if err := check(C.ghostty_terminal_get(t.ptr, C.GHOSTTY_TERMINAL_DATA_COLS,
		unsafe.Pointer(&v)), "reading cols"); err != nil {
		return 0, 0, err
	}
	return rows, uint16(v), nil
}

// mode reports whether a terminal mode is set.
func (t *Terminal) mode(m C.GhosttyMode) (bool, error) {
	var set C.bool
	if err := check(C.ghostty_terminal_mode_get(t.ptr, m, &set), "reading mode"); err != nil {
		return false, err
	}
	return bool(set), nil
}

// setMode turns a terminal mode on or off.
func (t *Terminal) setMode(m C.GhosttyMode, on bool) error {
	return check(C.ghostty_terminal_mode_set(t.ptr, m, C.bool(on)), "setting mode")
}

// screenY converts a grid reference to its row in screen coordinates, which count from the top
// of scrollback rather than the top of the viewport.
//
// Reports false when the reference cannot be expressed in screen coordinates, which callers
// treat as "cannot determine" rather than as an error.
func (t *Terminal) screenY(ref C.GhosttyGridRef) (uint32, bool, error) {
	var coord C.GhosttyPointCoordinate
	rc := C.ghostty_terminal_point_from_grid_ref(t.ptr, &ref, C.GHOSTTY_POINT_TAG_SCREEN, &coord)
	switch rc {
	case C.GHOSTTY_SUCCESS:
		return uint32(coord.y), true, nil
	case C.GHOSTTY_NO_VALUE, C.GHOSTTY_INVALID_VALUE:
		return 0, false, nil
	default:
		return 0, false, check(rc, "converting grid reference to a screen point")
	}
}
