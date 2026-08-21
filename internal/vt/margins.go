package vt

import "bytes"

// marginReportPrefix is the start of a DECRPM reply about left/right margin mode (private mode 69),
// and marginReportSuffix ends it. The full form is CSI ? 69 ; Ps $ y.
var (
	marginReportPrefix = []byte("\x1b[?69;")
	marginReportSuffix = []byte("$y")
)

// notRecognizedMarginReport is the reply cm gives when asked whether it supports left/right margins:
// Ps 0, "mode not recognized".
var notRecognizedMarginReport = []byte("\x1b[?69;0$y")

// DenyMarginMode rewrites a DECRPM reply about mode 69 to say the mode is not recognized.
//
// This is the fix for nvim scrolling both halves of a vertical split. The chain is long and every
// link was measured, because the symptom points nowhere near the cause.
//
// Left/right margin mode (DECLRMM, private mode 69) plus DECSLRM confine scrolling to a range of
// *columns*, which is what lets a program scroll one side of a vertical split without touching the
// other. cm's emulator implements it correctly, and that is the trap: cm answers mode queries from
// its own model, so it reported mode 69 as supported. The bytes that act on it are then forwarded
// verbatim to the real terminal, which does not implement it. kitty logs "Unsupported screen mode"
// and drops the DECSLRM, so the insert-line and delete-line operations that follow apply to the full
// width and scroll both splits. Measured in the same kitty window at the same moment: a program
// inside cm got "\x1b[?69;2$y" where bare kitty and zmx both gave "\x1b[?69;0$y".
//
// Ps 2 (reset) is as damaging as Ps 1 (set) here, which is the counter-intuitive part and the reason
// reporting real model state is not an option. nvim's tui_handle_term_mode sets
// has_left_and_right_margin_mode for set, permanently_set, *and* reset: the flag records that the
// mode can be changed, not that it is on. So any answer except 0 or permanently-reset enables the
// path. nvim latches it once at startup and never re-probes, which is why the damage outlives the
// query and shows up as every later scroll being wrong, keyboard as much as mouse.
//
// 0 rather than 4 (permanently reset). Both suppress the behavior, and 0 is what a terminal without
// the capability actually says: kitty answers 0, and tmux answers 0 from the default arm of its
// DECRQM switch for every mode it does not know, mode 69 included. Matching the terminals rather
// than inventing an answer keeps cm's reply indistinguishable from the truth.
//
// What this costs is nvim's terminal-side scroll optimization in a vertical split, where it repaints
// the moved rows instead. That is not a regression against the alternative: nvim's can_scroll still
// allows a scroll whenever the region is full width, so an unsplit window and a horizontal split are
// untouched, and the only case that changes is the one currently rendering incorrectly. It is also
// exactly where zmx already sits, since zmx proxies DECRQM to the terminal and therefore relays
// kitty's own 0.
//
// Deliberately a rewrite of the reply rather than a strip of the DECSLRM bytes on the way out.
// Removing bytes from the output stream desynchronizes the shim's numbering from the server's, and
// that has already cost a bug where a reconnecting client resumed inside an escape sequence: see
// docs/architecture.md on the two sequence-number spaces. A reply cm generates itself is not in the
// log, so rewriting one moves no positions.
//
// Returns data unchanged, without allocating, when there is nothing to rewrite. Every reply the
// emulator produces passes through here.
func DenyMarginMode(data []byte) []byte {
	// A scan with an explicit cursor rather than recursion. Recursion needed a way to tell "the tail was
	// rewritten" from "the tail was returned as-is", and the obvious test -- comparing lengths -- is
	// wrong here, because the replacement is the same length as what it replaces. That let a malformed
	// report hide a real one behind it.
	var out []byte
	start := 0
	for pos := 0; ; {
		rel := bytes.Index(data[pos:], marginReportPrefix)
		if rel < 0 {
			break
		}
		i := pos + rel
		after := i + len(marginReportPrefix)

		// The parameter sits between the prefix and the "$y". Only a well-formed report is rewritten,
		// since this also sees ordinary output that happens to contain the prefix, and a malformed one
		// is skipped rather than abandoning the rest of the chunk.
		rest := data[after:]
		end := bytes.Index(rest, marginReportSuffix)
		if end < 0 {
			break
		}
		if !allDigits(rest[:end]) {
			pos = after
			continue
		}

		out = append(out, data[start:i]...)
		out = append(out, notRecognizedMarginReport...)
		start = after + end + len(marginReportSuffix)
		pos = start
	}

	if out == nil {
		// Nothing to rewrite, which is the overwhelmingly common case: every reply the emulator
		// produces passes through here. Returned as-is so the fast path does not allocate.
		return data
	}
	return append(out, data[start:]...)
}

// allDigits reports whether p is a non-empty run of ASCII digits, which is what a DECRPM parameter is.
//
// Empty counts as false, so "CSI ? 69 ; $ y" with no parameter is left alone: it names no state, and
// rewriting it would invent an answer to a question the emulator did not answer.
func allDigits(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for _, b := range p {
		if b < '0' || b > '9' {
			return false
		}
	}
	return true
}
