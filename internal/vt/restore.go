//go:build cgo

package vt

import (
	"bytes"
	"errors"
	"fmt"

	"go.mitchellh.com/libghostty"
)

// OnAltScreen reports whether the model is on the alternate screen.
//
// Exposed because the server has to know when a session *leaves* it. A client that attached while a
// full-screen program was running has no main-screen content, since Restore serializes the active screen
// and libghostty offers no way to reach the other one, so the transition out is the moment that client has
// to be repainted. See Session.feedTerminal.
func (t *Terminal) OnAltScreen() (bool, error) {
	if t.closed {
		return false, fmt.Errorf("terminal is closed")
	}
	return t.mode(libghostty.ModeAltScreenSave)
}

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
	syncMode := libghostty.ModeSyncOutput
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
	// Sent whichever screen the model is on, which is a change from sending it only for a main-screen
	// blob. The two directions looked asymmetric and are not.
	//
	// A blob for a session on the *alternate* screen does say where it belongs, via ?1049h from the mode
	// state, and that was taken to be enough. It is not, because saying where the content belongs says
	// nothing about the screen it does *not* belong to. The client's main screen is never written, so it
	// keeps whatever that window held before the attach, and the next ?1049l the program sends on its way
	// out pops the terminal onto it.
	//
	// The symptom: attach to a session running a full-screen program, quit the program, and the screen
	// fills with content from before the attach rather than the session's shell. Reproduced in
	// TestRestoreOnTheAlternateScreenSurvivesTheProgramExiting.
	//
	// Clearing main is the whole of what can be done here, and it is not the whole fix. cm cannot put the
	// session's own main screen into this blob, because libghostty serializes the active screen and there is
	// no API for the other one: GhosttyTerminalScreen is a read of which screen is active, not a selector.
	// So a client attaching mid-program still loses the shell history that was on screen before the program
	// started; what it no longer does is show a stranger's content instead. See the test for the remaining
	// half.
	screenPrefix := "\x1b[?1049l\x1b[2J\x1b[H"

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
	screenTop, ok, err := t.gridRef(libghostty.PointTagScreen, 0, 0)
	if err != nil || !ok {
		return false, err
	}
	activeTop, ok, err := t.gridRef(libghostty.PointTagActive, 0, 0)
	if err != nil || !ok {
		return false, err
	}

	// Equal refs mean the active area starts at the top of the screen, so there is no
	// scrollback to emit.
	if *screenTop == *activeTop {
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

	sbEnd, ok, err := t.gridRef(libghostty.PointTagScreen, cols-1, activeY-1)
	if err != nil || !ok {
		return false, err
	}

	out, err := t.format(formatOptions{
		emit:      libghostty.FormatterFormatVT,
		selection: &libghostty.Selection{Start: *screenTop, End: *sbEnd},
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
		emit: libghostty.FormatterFormatVT,
		// Modes, the scrolling region, and keyboard state are what make a restored screen
		// behave rather than merely look right.
		//
		// Both keyboard options are needed and they cover different sequences: keyboard is xterm's
		// modifyOtherKeys, kittyKeyboard is the kitty protocol's flags. Only the first was set for a
		// long time, and the names are close enough that the gap read as already handled. See
		// TestRestoreCarriesKittyKeyboardFlags.
		modes:           true,
		scrollingRegion: true,
		keyboard:        true,
		kittyKeyboard:   true,
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
	topLeft, okTL, err := t.gridRef(libghostty.PointTagActive, 0, 0)
	if err != nil {
		return err
	}
	rows, cols, err := t.Size()
	if err != nil {
		return err
	}
	var (
		bottomRight *libghostty.GridRef
		okBR        bool
	)
	for y := int(rows) - 1; y >= 0; y-- {
		ref, ok, err := t.gridRef(libghostty.PointTagActive, cols-1, uint32(y))
		if err != nil {
			return err
		}
		if ok {
			bottomRight, okBR = ref, true
			break
		}
	}

	if okTL && okBR {
		opts.selection = &libghostty.Selection{Start: *topLeft, End: *bottomRight}
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
	return t.format(formatOptions{emit: libghostty.FormatterFormatPlain})
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
		emit:   libghostty.FormatterFormatPlain,
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
		emit:  libghostty.FormatterFormatVT,
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
	return t.format(formatOptions{emit: libghostty.FormatterFormatHTML, style: true})
}

// VT returns the terminal's full contents, scrollback included, as escape sequences.
func (t *Terminal) VT() ([]byte, error) {
	if t.closed {
		return nil, fmt.Errorf("terminal is closed")
	}
	// The same state options as the restore blob, since this is how the state is read: `cm history
	// --format vt` is the only way to see what a fresh client would receive, so an option set here
	// that differs from emitViewport's makes this view lie about it.
	return t.format(formatOptions{
		emit:          libghostty.FormatterFormatVT,
		modes:         true,
		keyboard:      true,
		kittyKeyboard: true,
		cursor:        true,
		style:         true,
	})
}

// formatOptions mirrors the subset of the formatter's options cm uses, so callers above do not
// deal with C structs.
type formatOptions struct {
	emit            libghostty.FormatterFormat
	unwrap          bool
	trim            bool
	selection       *libghostty.Selection
	modes           bool
	scrollingRegion bool
	keyboard        bool
	kittyKeyboard   bool
	tabstops        bool
	palette         bool
	pwd             bool
	cursor          bool
	style           bool
	hyperlink       bool
}

// format runs the formatter and returns its output.
//
// Every option is passed explicitly, false included, rather than only the ones set. The binding leaves
// an option it was not given at libghostty's default, and cm's restore depends on several being off:
// tabstops move the cursor after it has been positioned, and the palette overrides the user's theme.
func (t *Terminal) format(o formatOptions) ([]byte, error) {
	opts := []libghostty.FormatterOption{
		libghostty.WithFormatterFormat(o.emit),
		libghostty.WithFormatterUnwrap(o.unwrap),
		libghostty.WithFormatterTrim(o.trim),
		libghostty.WithFormatterExtraModes(o.modes),
		libghostty.WithFormatterExtraScrollingRegion(o.scrollingRegion),
		libghostty.WithFormatterExtraKeyboard(o.keyboard),
		libghostty.WithFormatterExtraTabstops(o.tabstops),
		libghostty.WithFormatterExtraPalette(o.palette),
		libghostty.WithFormatterExtraPwd(o.pwd),
		libghostty.WithFormatterExtraKittyKeyboard(o.kittyKeyboard),
		libghostty.WithFormatterExtraCursor(o.cursor),
		libghostty.WithFormatterExtraStyle(o.style),
		libghostty.WithFormatterExtraHyperlink(o.hyperlink),
	}
	if o.selection != nil {
		opts = append(opts, libghostty.WithFormatterSelection(o.selection))
	}

	f, err := libghostty.NewFormatter(t.term, opts...)
	if err != nil {
		return nil, check(err, "creating formatter")
	}
	defer f.Close()

	out, err := f.Format()
	if err != nil {
		return nil, check(err, "formatting terminal")
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// gridRef resolves a point to a grid reference, reporting whether it exists.
//
// An out-of-bounds point is not an error: an empty or short screen legitimately has no cell at
// a given position, and callers fall back rather than fail.
func (t *Terminal) gridRef(tag libghostty.PointTag, x uint16, y uint32) (*libghostty.GridRef, bool, error) {
	ref, err := t.term.GridRef(libghostty.Point{Tag: tag, X: x, Y: y})
	switch {
	case err == nil:
		return ref, true, nil
	case errors.Is(err, libghostty.ErrInvalidValue), errors.Is(err, libghostty.ErrNoValue):
		return nil, false, nil
	default:
		return nil, false, check(err, "resolving grid reference")
	}
}

// Size reports the terminal's dimensions.
func (t *Terminal) Size() (rows, cols uint16, err error) {
	rows, err = t.term.Rows()
	if err != nil {
		return 0, 0, check(err, "reading rows")
	}
	cols, err = t.term.Cols()
	if err != nil {
		return 0, 0, check(err, "reading cols")
	}
	return rows, cols, nil
}

// mode reports whether a terminal mode is set.
func (t *Terminal) mode(m libghostty.Mode) (bool, error) {
	on, err := t.term.Mode(m)
	return on, check(err, "reading mode")
}

// setMode turns a terminal mode on or off.
func (t *Terminal) setMode(m libghostty.Mode, on bool) error {
	return check(t.term.SetMode(m, on), "setting mode")
}

// screenY converts a grid reference to its row in screen coordinates, which count from the top
// of scrollback rather than the top of the viewport.
//
// Reports false when the reference cannot be expressed in screen coordinates, which callers
// treat as "cannot determine" rather than as an error.
func (t *Terminal) screenY(ref *libghostty.GridRef) (uint32, bool, error) {
	pt, err := t.term.PointFromGridRef(ref, libghostty.PointTagScreen)
	switch {
	case err == nil:
		return pt.Y, true, nil
	case errors.Is(err, libghostty.ErrNoValue), errors.Is(err, libghostty.ErrInvalidValue):
		return 0, false, nil
	default:
		return 0, false, check(err, "converting grid reference to a screen point")
	}
}
