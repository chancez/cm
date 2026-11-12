//go:build cgo

package vt

import (
	"fmt"

	"go.mitchellh.com/libghostty"
)

// resettableModes is every mode the bindings define except the ones that are actions rather than state:
// the alternate screens and save cursor switch screens or move the cursor, and 132 column resizes and
// clears. ResetModes leaves the alternate screen itself.
var resettableModes = []libghostty.Mode{
	libghostty.ModeDisableKeyboard,
	libghostty.ModeInsert,
	libghostty.ModeSendReceiveMode,
	libghostty.ModeLinefeed,
	libghostty.ModeCursorKeys,
	libghostty.ModeSlowScroll,
	libghostty.ModeReverseColors,
	libghostty.ModeOrigin,
	libghostty.ModeWraparound,
	libghostty.ModeAutorepeat,
	libghostty.ModeMouseEventX10,
	libghostty.ModeCursorBlinking,
	libghostty.ModeCursorVisible,
	libghostty.ModeEnableMode3,
	libghostty.ModeReverseWrap,
	libghostty.ModeKeypadKeys,
	libghostty.ModeBackarrowKeyMode,
	libghostty.ModeEnableLeftAndRightMargin,
	libghostty.ModeMouseEventNormal,
	libghostty.ModeMouseEventButton,
	libghostty.ModeMouseEventAny,
	libghostty.ModeFocusEvent,
	libghostty.ModeMouseFormatUTF8,
	libghostty.ModeMouseFormatSGR,
	libghostty.ModeMouseAlternateScroll,
	libghostty.ModeMouseFormatURxvt,
	libghostty.ModeMouseFormatSGRPixels,
	libghostty.ModeIgnoreKeypadWithNumlock,
	libghostty.ModeAltEscPrefix,
	libghostty.ModeAltSendsEscape,
	libghostty.ModeReverseWrapExtended,
	libghostty.ModeBracketedPaste,
	libghostty.ModeSynchronizedOutput,
	libghostty.ModeGraphemeCluster,
	libghostty.ModeReportColorScheme,
	libghostty.ModeReportVisibility,
	libghostty.ModeInBandSizeReports,
	libghostty.ModeKittyPasteEvents,
}

// resetState is what a program sets that is not a mode. Popping more kitty keyboard entries than any
// program pushes empties the stack, which resets the flags. The scroll region is reset between a cursor
// save and restore because DECSTBM homes the cursor, which would put the next output over the content;
// the charset and pen follow the restore, since DECRC brings back both.
const resetState = "\x1b[<99u\x1b7\x1b[r\x1b8\x1b(B\x1b[0m\x1b[>4m\x1b[0 q"

// ResetModes puts every mode back to the value a new terminal starts with, leaving the content.
//
// For a model whose program is gone without its teardown, which is what a reboot leaves in a persisted
// log. nvim's modes left an attaching client in kitty keyboard flags 3, so kitty sent ctrl-c as CSI 99;5u
// plus a release event, the new zsh read both as typing, and the session took no keystroke it understood.
// tmux does the same on respawn-pane (screen_reinit), resetting to fixed defaults rather than to a list of
// modes known to cause trouble, which is the list that would go stale.
//
// The alternate screen is left too, and its content goes with it, as kitty's and iTerm2's restores and
// tmux's respawn do. Nothing remains to own it, a shell drawing there has no scrollback, and a frozen
// editor above a prompt reads as a live one. Left only when the model is on it, since from the main screen
// ?1049l restores whatever cursor was last saved; resetState saving the cursor first also covers that, so
// the guard is the explicit half of it. Each screen keeps its own keyboard stack, so the alternate one is
// emptied before leaving it.
//
// The defaults are read from a new terminal rather than written down, so they cannot drift from
// libghostty's. Each mode is written as DECSET or DECRST rather than set through the API, so libghostty
// runs the same handling a program's sequence gets. The modes are compared after the screen is left,
// since leaving it changes them.
func (t *Terminal) ResetModes() error {
	if t.closed {
		return fmt.Errorf("terminal is closed")
	}
	fresh, err := New(1, 1, Callbacks{})
	if err != nil {
		return err
	}
	defer fresh.Close()

	alt, err := t.OnAltScreen()
	if err != nil {
		return err
	}
	var seq []byte
	if alt {
		seq = append(seq, resetState+"\x1b[?1049l"...)
	}
	seq = append(seq, resetState...)
	if err := t.Write(seq); err != nil {
		return err
	}
	seq = seq[:0]
	for _, m := range resettableModes {
		want, err := fresh.mode(m)
		if err != nil {
			return err
		}
		got, err := t.mode(m)
		if err != nil {
			return err
		}
		switch {
		case got == want:
			// Only what differs is written, because setting some modes acts as well, and the act is
			// wrong when nothing changed.
		case m == libghostty.ModeOrigin:
			// DECOM homes the cursor whichever way it is set, which would put the next output over the
			// content, so it alone is set without a sequence.
			if err := t.setMode(m, want); err != nil {
				return err
			}
		default:
			seq = append(seq, modeSequence(m, want)...)
		}
	}
	return t.Write(seq)
}

// modeSequence is the SM or RM sequence, ANSI or DEC private, that sets m to on.
func modeSequence(m libghostty.Mode, on bool) string {
	final := 'l'
	if on {
		final = 'h'
	}
	if m.ANSI() {
		return fmt.Sprintf("\x1b[%d%c", m.Value(), final)
	}
	return fmt.Sprintf("\x1b[?%d%c", m.Value(), final)
}
