package server

import (
	"bytes"
	"fmt"
)

// FindingMarginModeClaimed is a terminal model that reports left/right margin support to programs.
const FindingMarginModeClaimed = "margin-mode-claimed"

// marginModeQuery is DECRQM for private mode 69, byte for byte what nvim sends at startup.
const marginModeQuery = "\x1b[?69$p"

// marginModeDenied is the reply that suppresses the behavior: Ps 0, "mode not recognized".
const marginModeDenied = "\x1b[?69;0$y"

// checkMarginMode reports a terminal model that tells programs cm can do left/right margins.
//
// This encodes a bug whose symptom named the wrong component in three different ways, which is the bar
// for a doctor check. What the user sees is nvim scrolling *both* halves of a vertical split when only
// one should move. That reads as an nvim bug, or as a terminal bug, and nothing about it suggests a
// multiplexer answering a capability question. It also survives quitting nvim, since the answer is
// given once at startup and latched, so "it started happening and now it always happens" is the
// report.
//
// Left/right margin mode (DECLRMM, private mode 69) plus DECSLRM confine scrolling to a range of
// columns. libghostty implements it, most terminals do not, and cm forwards the sequences that act on
// it verbatim, so a program told "yes" emits margins that the real terminal drops and the insert-line
// and delete-line operations that follow apply full width. See docs/architecture.md.
//
// Probed rather than inferred from a build flag or a version, following checkEmulatorSpeed. The reply
// is the thing that matters, so asking for it stays correct if the mechanism moves: a libghostty that
// changed its default, or a rewrite that stopped being wired into the callback, both surface here
// without this check being updated. A test asserting the rewrite in isolation cannot say that.
//
// Any reply other than 0 is a finding, not just the ";2" cm actually produced. nvim treats set, reset,
// and permanently-set alike, because the flag it keeps records that the mode can be *changed* rather
// than that it is on, so a report of "reset" enables the path exactly as "set" does. Ps 4,
// permanently-reset, is accepted since it also tells a program the mode cannot be turned on.
//
// Costs one terminal and one short write, so it runs unconditionally.
func (m *Manager) checkMarginMode() []Finding {
	if m.newTerminal == nil {
		// No emulator at all, which checkTerminal already reports.
		return nil
	}

	term, err := m.newTerminal(24, 80)
	if err != nil {
		// Reported wherever a session needs a model. Attributing it to margins would mislead.
		return nil
	}
	defer term.Close()

	if err := term.Write([]byte(marginModeQuery)); err != nil {
		return nil
	}

	var reply []byte
	for _, pending := range term.TakePending() {
		if bytes.Contains(pending, []byte("\x1b[?69;")) {
			reply = pending
			break
		}
	}
	if reply == nil {
		// No answer at all is the honest state for a capability cm does not have, and it is what a
		// terminal that ignores the query does. Nothing to report.
		return nil
	}
	if bytes.Contains(reply, []byte(marginModeDenied)) || bytes.Contains(reply, []byte("\x1b[?69;4$y")) {
		return nil
	}

	return []Finding{{
		Kind: FindingMarginModeClaimed,
		Detail: fmt.Sprintf(
			"the terminal model answered %q when asked whether it supports left/right margins "+
				"(DECRQM private mode 69), where %q is expected. A program told the mode is available "+
				"uses DECSLRM to scroll part of a row range, and cm forwards that to a terminal that "+
				"usually ignores it, so the scroll applies to the full width instead. The visible "+
				"symptom is nvim scrolling both halves of a vertical split, and it persists for the "+
				"life of the program because the answer is given once at startup",
			reply, marginModeDenied),
		Fixable: false,
	}}
}
