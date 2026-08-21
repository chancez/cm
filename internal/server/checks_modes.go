package server

import (
	"bytes"
	"fmt"
)

// FindingModeClaimed is a terminal model that reports a capability to programs that cm cannot honor.
const FindingModeClaimed = "mode-claimed"

// deniedModeCheck is one private mode cm must report as not recognized, with enough of the incident to
// make a finding actionable.
type deniedModeCheck struct {
	// mode is the private mode number, used to build the query and to match the reply.
	mode int
	// what names the capability in the terminology a program's documentation would use.
	what string
	// symptom is what the user sees when cm claims the mode, which is the part that points somewhere
	// other than the cause.
	symptom string
}

// deniedModeChecks mirrors vt.deniedModes. Kept as its own list rather than read from vt because the
// useful part here is the symptom text, which is diagnostic prose rather than emulator behavior.
//
// Duplicating the numbers is deliberate and is what makes the check independent: sharing the list would
// make a check that passes whenever the two agree, including when both are wrong. This way a mode
// dropped from vt's list fails here.
var deniedModeChecks = []deniedModeCheck{
	{
		mode: 69,
		what: "left/right margins (DECRQM private mode 69)",
		symptom: "A program told the mode is available uses DECSLRM to scroll part of a row range, " +
			"and cm forwards that to a terminal that usually ignores it, so the scroll applies to " +
			"the full width instead. The visible symptom is nvim scrolling both halves of a " +
			"vertical split",
	},
	{
		mode: 2048,
		what: "in-band size reports (DECRQM private mode 2048)",
		// cm does now send these reports, from its resize path, so the mode is denied and honored at
		// once. The denial still matters: it keeps cm's answer consistent with what tmux says and stops
		// a program relying on a promise the *model* would make on cm's behalf, since the model's own
		// report is generated inside the emulator's resize and arrives out of turn. What made this a
		// real bug was nvim setting the mode without waiting for any answer, so the report is what
		// fixes it and this check only guards the reply.
		symptom: "A program told the mode is available stops relying on SIGWINCH and waits to be told " +
			"about each resize in band. cm sends those reports itself, from the size it sets, so the " +
			"reply here should still say the mode is unavailable: answering otherwise means the model " +
			"is promising on cm's behalf, and the model's own reports arrive out of turn. The visible " +
			"symptom of that going wrong is nvim keeping its old height after a terminal split closes",
	},
}

// checkDeniedModes reports a terminal model that tells programs cm has a capability it cannot honor.
//
// This encodes two bugs whose symptoms named the wrong component in every direction, which is the bar
// for a doctor check. What the user sees is nvim scrolling both halves of a vertical split, or nvim
// refusing to grow when a split closes. Both read as an nvim bug, or as a terminal bug, and nothing
// about either suggests a multiplexer answering a capability question. Both also survive quitting nvim,
// since the answer is given once at startup and latched, so "it started happening and now it always
// happens" is the report.
//
// Probed rather than inferred from a build flag or a version, following checkEmulatorSpeed. The reply is
// the thing that matters, so asking for it stays correct if the mechanism moves: a libghostty that
// changed its default, or a rewrite that stopped being wired into the callback, both surface here
// without this check being updated. A test asserting the rewrite in isolation cannot say that.
//
// Any reply other than 0 is a finding, not just the ";2" cm actually produced. nvim treats set, reset,
// and permanently-set alike, because the flag it keeps records that the mode can be *changed* rather
// than that it is on, so a report of "reset" enables the path exactly as "set" does. Ps 4,
// permanently-reset, is accepted since it also tells a program the mode cannot be turned on.
//
// Costs one terminal and one short write per mode, so it runs unconditionally.
func (m *Manager) checkDeniedModes() []Finding {
	if m.newTerminal == nil {
		// No emulator at all, which checkTerminal already reports.
		return nil
	}

	var findings []Finding
	for _, check := range deniedModeChecks {
		findings = append(findings, m.checkDeniedMode(check)...)
	}
	return findings
}

// checkDeniedMode probes one mode.
//
// A fresh terminal per mode, so a model left in an odd state by one query cannot change the answer to
// the next.
func (m *Manager) checkDeniedMode(check deniedModeCheck) []Finding {
	term, err := m.newTerminal(24, 80)
	if err != nil {
		// Reported wherever a session needs a model. Attributing it to one mode would mislead.
		return nil
	}
	defer term.Close()

	if err := term.Write([]byte(fmt.Sprintf("\x1b[?%d$p", check.mode))); err != nil {
		return nil
	}

	// Matched on this mode's own prefix, so another mode's report cannot be mistaken for it.
	prefix := []byte(fmt.Sprintf("\x1b[?%d;", check.mode))
	var reply []byte
	for _, pending := range term.TakePending() {
		if bytes.Contains(pending, prefix) {
			reply = pending
			break
		}
	}
	if reply == nil {
		// No answer at all is the honest state for a capability cm does not have, and it is what a
		// terminal that ignores the query does. Nothing to report.
		return nil
	}

	denied := fmt.Sprintf("\x1b[?%d;0$y", check.mode)
	permanentlyReset := fmt.Sprintf("\x1b[?%d;4$y", check.mode)
	if bytes.Contains(reply, []byte(denied)) || bytes.Contains(reply, []byte(permanentlyReset)) {
		return nil
	}

	return []Finding{{
		Kind: FindingModeClaimed,
		Detail: fmt.Sprintf(
			"the terminal model answered %q when asked whether it supports %s, where %q is "+
				"expected. %s, and it persists for the life of the program because the answer is "+
				"given once at startup",
			reply, check.what, denied, check.symptom),
		Fixable: false,
	}}
}
