package vt

import (
	"bytes"
	"strconv"
)

// deniedModes are the private modes cm reports as not recognized, whatever the model's real state is.
//
// The category, and why it is not simply "modes cm does not implement": every mode here is one the
// *emulator* implements correctly. What makes them different is where the effect lands. An ordinary
// mode query is answered from the model because the model is what the program is talking to. These
// are answerable by the model too, and the answer is still wrong, because saying yes makes the program
// change what it emits and the consequence lives somewhere the model does not control.
//
// The test for membership: does the reply make the program rely on something outside the model? Two
// ways that has happened, one per member, and they fail in opposite directions:
//
//   - Mode 69 makes the program emit sequences whose effect belongs to the real terminal. cm forwards
//     DECSLRM verbatim to a terminal that drops it.
//   - Mode 2048 makes the program stop listening to the pty and wait for notifications from cm that cm
//     never sends.
//
// Both were found from a symptom rather than by auditing the mode list, and neither was predicted, so
// a new entry is expected to arrive the same way. Adding one is a deliberate act: name the incident,
// because a mode denied without a reason is one the next person will re-enable.
var deniedModes = []int{
	// DECLRMM, left/right margins. nvim scrolled both halves of a vertical split.
	69,
	// In-band size reports. nvim stopped resizing when a kitty split closed.
	2048,
}

// modeReport holds one denied mode's byte patterns, precomputed so the scan does no formatting. A
// DECRPM reply has the form CSI ? Pm ; Ps $ y.
type modeReport struct {
	prefix []byte
	denied []byte
}

// reportSuffix ends a DECRPM reply.
var reportSuffix = []byte("$y")

// modeReports is deniedModes in the form the scan uses.
var modeReports = func() []modeReport {
	reports := make([]modeReport, 0, len(deniedModes))
	for _, mode := range deniedModes {
		n := strconv.Itoa(mode)
		reports = append(reports, modeReport{
			prefix: []byte("\x1b[?" + n + ";"),
			// Ps 0, "mode not recognized".
			denied: []byte("\x1b[?" + n + ";0$y"),
		})
	}
	return reports
}()

// sizeReportPrefix starts an in-band size report, CSI 48 ; rows ; cols ; ypixel ; xpixel t. Mode 2048
// is what turns these on, and dropSizeReports removes them.
var sizeReportPrefix = []byte("\x1b[48;")

// SizeReport builds the in-band size report for a size, the sequence mode 2048 promises.
//
// Pixel dimensions are reported as 0, which is the honest answer rather than a placeholder: cm has a
// grid in cells and no font, so it does not know how large a cell is. Zero is the value kitty itself
// sends when it cannot determine pixel size, and the sequence's own definition allows it, so a program
// reading it sees "unknown" rather than a number cm made up. nvim only reads the cell fields.
//
// Built here rather than in the server because this package owns every byte cm sends a program on the
// emulator's behalf, and because dropSizeReports next door has to recognize the same shape. Keeping the
// producer and the recognizer in one file is what stops them drifting apart.
func SizeReport(rows, cols uint16) []byte {
	out := make([]byte, 0, len(sizeReportPrefix)+16)
	out = append(out, sizeReportPrefix...)
	out = strconv.AppendUint(out, uint64(rows), 10)
	out = append(out, ';')
	out = strconv.AppendUint(out, uint64(cols), 10)
	out = append(out, ";0;0t"...)
	return out
}

// DenyModes rewrites DECRPM replies about the modes in deniedModes to say the mode is not recognized,
// and drops any in-band size report the model produced.
//
// Two bugs share this mechanism, both diagnosed through a long chain because the symptom points
// nowhere near the cause. What they have in common is cm answering a capability question from its own
// model when the model is not what decides the answer.
//
// **Mode 69, left/right margins (DECLRMM).** The mode plus DECSLRM confine scrolling to a range of
// *columns*, which is how a program scrolls one side of a vertical split without touching the other.
// libghostty implements it, and that is the trap: the bytes acting on it are forwarded verbatim to the
// real terminal, which does not. kitty logs "Unsupported screen mode" and drops the DECSLRM, so the
// insert-line and delete-line operations that follow apply full width. The symptom was nvim scrolling
// both halves of a vertical split.
//
// **Mode 2048, in-band size reports.** A terminal setting this promises to report every resize as
// CSI 48 ; rows ; cols ; ypixel ; xpixel t, and a program given that promise stops relying on
// SIGWINCH. cm answered "supported" and then never sent a report: libghostty emits them from
// StreamHandler.resize, gated on the mode, while cm resizes through ghostty_terminal_resize. So the
// pty was resized correctly and nvim was no longer listening to it. The symptom was nvim keeping half
// the window after a kitty split closed, until something else forced a redraw. Measured against a fake
// terminal answering the probe directly, holding everything else fixed: told ";2" nvim emitted 0 bytes
// on both a shrink and a grow, and told ";0" it emitted 4302 and 11206.
//
// Ps 2 (reset) is as damaging as Ps 1 (set) for both, which is the counter-intuitive part and the
// reason reporting real model state is not an option. nvim records only that a mode *can be changed*:
// tui_handle_term_mode sets has_left_and_right_margin_mode for set, permanently-set, and reset alike,
// and the 2048 handshake behaves the same way. cm answered ";2" in both cases, so a fix suppressing
// only ";1" would have changed nothing. Each mode is probed once at startup and latched, which is why
// the damage outlives the query and reads as "it always does this now".
//
// 0 rather than 4 (permanently reset). Both suppress the behavior, and 0 is what a terminal without
// the capability actually says: kitty answers 0 for mode 69, and tmux answers 0 from the default arm
// of its DECRQM switch for every mode it does not know, both of these included. Matching the terminals
// rather than inventing an answer keeps cm's reply indistinguishable from the truth.
//
// Mode 2048 is denied *and* honored, which reads like a contradiction and is the only combination that
// works. The denial stops a program that asks and believes the answer. nvim is not that program: it
// sends CSI ? 2048 h in the same startup burst as 2026, 2027, and 2031, without waiting for the reply
// to the query it also sends. Measured by relaying nvim's own output stream: one query, one set, no
// reset. So the DECRQM answer cannot be the whole mechanism, and cm has to keep the promise for any
// program that sets the mode regardless of what it was told.
//
// An earlier version of this comment argued cm could not honor the mode because "kitty's own reports
// arrive on the client's input stream where IsQueryReply discards them, so emitting reports outbound
// would not fix that half". Both halves were measured and it is wrong. Two DECRPM replies really do
// exist, cm's ";0" and the client's ";2", and cm's wins: the program received "\x1b[?2048;0$y" while
// the debug log recorded "\x1b[?2048;2$y" being discarded from the client. The inbound half is already
// correct, so honoring the mode is only the outbound half.
//
// Where the multiplexers sit, all three measured with the same probe in the same terminal:
//
//   - zmx answers ";2" and its reports work, because it answers nothing itself and passes the leader
//     client's bytes straight to the pty. kitty answered, kitty reports, zmx is a pipe.
//   - tmux never implements the mode: it is absent from its DECRQM switch, so it falls to the default
//     "not recognized" arm, and it takes its size from TIOCGWINSZ instead. It also consumes a client's
//     CSI ... t for its own bookkeeping and never forwards one to a pane, gated on TTY_WINSIZEQUERY
//     with the comment "If we did not request this, ignore it".
//   - cm cannot be either. It is not a pipe, because it answers what its model can answer and filters
//     client replies to stop a query being answered twice. And it cannot ignore the mode the way tmux
//     does, because programs inside cm enable it anyway.
//
// So cm emits the reports itself, from the size it just set, which is the one place that knows every
// resize. That is what makes the promise keepable: a report generated at the resize covers changes that
// never pass through a client at all, which is the case a forwarded client report would miss.
//
// Deliberately a rewrite of the reply rather than a strip of the acting bytes on the way out. Removing
// bytes from the output stream desynchronizes the shim's numbering from the server's, and that has
// already cost a bug where a reconnecting client resumed inside an escape sequence: see
// docs/architecture.md on the two sequence-number spaces. A reply cm generates itself is not in the
// log, so rewriting one moves no positions.
//
// Returns data unchanged, without allocating, when there is nothing to rewrite. Every reply the
// emulator produces passes through here.
func DenyModes(data []byte) []byte {
	// Each mode is scanned for separately rather than parsing every DECRPM reply, because the
	// overwhelmingly common case is a chunk containing none of them and bytes.Index is what makes that
	// case free. A second pass over a chunk that already matched is not worth avoiding.
	for i := range modeReports {
		data = denyMode(data, &modeReports[i])
	}
	return dropSizeReports(data)
}

// dropSizeReports removes in-band size reports the model generated.
//
// Denying mode 2048 stops a program that *asks* from enabling it, and this covers the program that sets
// it without asking, which the DECRPM rewrite alone does not reach. libghostty honors the mode
// faithfully: with it set, a resize makes the model emit CSI 48 ; ... t, found by a test asserting that
// a resize queues nothing for the pty. Nothing drains the emulator's queue on the resize path, so the
// report was not reaching the pty promptly; it sat in the queue until the next output arrived and was
// then delivered as though it answered whatever query came after it. That is the same class of bug as
// the recorded `wallfacer -h` corruption, where a reply arriving out of turn was consumed as the answer
// to a different question.
//
// Still dropped now that cm emits its own reports, and dropping is what makes emitting correct rather
// than being in tension with it. The model's report is untimely: it is produced inside the emulator's
// resize and sits in the queue until the next output arrives, which is how it came to be delivered as
// the answer to a later query. cm's own report is sent from the resize path at the moment of the
// resize, in order, through the same queue as every other reply. Keeping both would send two reports
// per resize, and the model's would be the one arriving out of turn.
//
// So the division is: the model decides *whether* a report is owed, since it tracks the mode the
// program set, and the server decides *when* one is sent. See SizeReport and Session.resize.
//
// Safe to drop here for the reason the rewrite is safe: these are bytes the model generated for the pty,
// not session output, so they were never in the log and removing one moves no sequence numbers. See
// docs/architecture.md on the two numbering spaces.
func dropSizeReports(data []byte) []byte {
	// found rather than a nil check on out, because dropping is a deletion: a chunk that is nothing but
	// one report leaves out empty, and "empty" and "nothing matched" are different answers. Testing
	// out == nil returned the report unchanged, which is the whole bug this function exists to fix.
	found := false
	var out []byte
	start := 0
	for pos := 0; ; {
		rel := bytes.Index(data[pos:], sizeReportPrefix)
		if rel < 0 {
			break
		}
		i := pos + rel
		after := i + len(sizeReportPrefix)

		// A size report's parameters are digits and semicolons ending in "t". Anything else carrying the
		// prefix is left alone, matching the rewrite: this sees only model-generated bytes today, and a
		// malformed sequence must not be truncated into something that parses.
		rest := data[after:]
		end := bytes.IndexByte(rest, 't')
		if end < 0 {
			break
		}
		if !allDigitsOrSemicolons(rest[:end]) {
			pos = after
			continue
		}

		found = true
		out = append(out, data[start:i]...)
		start = after + end + 1
		pos = start
	}

	if !found {
		return data
	}
	return append(out, data[start:]...)
}

// allDigitsOrSemicolons reports whether p is a non-empty run of ASCII digits and semicolons, which is
// what an in-band size report's parameter list is.
func allDigitsOrSemicolons(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for _, b := range p {
		if (b < '0' || b > '9') && b != ';' {
			return false
		}
	}
	return true
}

// denyMode rewrites every well-formed DECRPM reply for one mode.
func denyMode(data []byte, report *modeReport) []byte {
	// A scan with an explicit cursor rather than recursion. Recursion needed a way to tell "the tail was
	// rewritten" from "the tail was returned as-is", and the obvious test -- comparing lengths -- is
	// wrong here, because the replacement can be the same length as what it replaces. That let a
	// malformed report hide a real one behind it.
	var out []byte
	start := 0
	for pos := 0; ; {
		rel := bytes.Index(data[pos:], report.prefix)
		if rel < 0 {
			break
		}
		i := pos + rel
		after := i + len(report.prefix)

		// The parameter sits between the prefix and the "$y". Only a well-formed report is rewritten,
		// since this also sees ordinary output that happens to contain the prefix, and a malformed one
		// is skipped rather than abandoning the rest of the chunk.
		rest := data[after:]
		end := bytes.Index(rest, reportSuffix)
		if end < 0 {
			break
		}
		if !allDigits(rest[:end]) {
			pos = after
			continue
		}

		out = append(out, data[start:i]...)
		out = append(out, report.denied...)
		start = after + end + len(reportSuffix)
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
