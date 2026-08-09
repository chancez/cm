package osc

import (
	"bytes"
	"strings"
)

// ReportNumber is the OSC number cm claims for its own shell integration.
//
// 25453 is 0x636d, which is ASCII "cm". That is the whole reason for it: a number nobody else has a
// motive to pick, and one whose origin is evident to anyone who wonders why it was chosen.
//
// Picked against the set actually in use rather than by hoping. The pinned libghostty parser enumerates
// what terminals recognize in practice -- 1, 2, 3, 5, 6, 7, 8, 9, 10, 11, 13, 21, 22, 30, 52, 55, 66, 72,
// 77, 104, 133, 300, 552, 777, 1337, 3008, 5522 -- and this collides with none of them. It also avoids the
// numbers a terminal is likeliest to grow into: the low two-digit space where new standard sequences land,
// and the vendor blocks already claimed by iTerm2 (1337), the notification extension (777), and the
// hierarchical-context spec (3008).
//
// Under 65536 on purpose. A parser that reads the number into a u16 wraps anything larger, so a
// six-digit private number risks being silently mistaken for a real sequence -- a failure that would look
// like corruption rather than like a rejected sequence.
//
// An unrecognized OSC is discarded by every terminal that follows the spec, including the outer terminal
// cm's own output passes through, so emitting this into a pty that is not cm's is inert rather than
// damaging. That property is what makes it safe for a shell to emit unconditionally.
const ReportNumber = 25453

// reportIntro is the introducer every cm report begins with.
const reportIntro = "\x1b]25453;"

// Report is what a shell integration told cm about itself.
//
// Distinct from a CommandState, which is derived from OSC 133 and describes a command. This carries what
// OSC 133 cannot express, so the two are complementary rather than alternatives.
type Report struct {
	// State is what the shell says it is doing: "busy", "blocked", "idle", or "clear" to withdraw a
	// previous report. Empty when the report carried no state.
	State string
	// Detail is a short note to show alongside the state.
	Detail string
	// Source names what reported, so one reporter is distinguishable from another.
	Source string
}

// reportStates is the set a report may carry.
//
// A fixed set rather than any string, so a typo in a shell hook is ignored rather than becoming a state
// nothing can wait for. The shell integration is edited by hand, which is exactly where typos come from.
var reportStates = map[string]bool{
	"busy":    true,
	"blocked": true,
	"idle":    true,
	"clear":   true,
}

// ReportTracker follows cm's own OSC reports across a stream of output.
//
// Stateful for the same reason CommandTracker is: a pty read is bounded by the kernel buffer rather than
// by anything the shell intends, so a sequence can be split across reads. A stateless matcher would drop
// exactly the reports that arrive at a chunk boundary, which is a rare enough case to survive testing and
// common enough to matter in use.
//
// Not safe for concurrent use. In cm only the output pump feeds one.
type ReportTracker struct {
	// last holds the most recent report, and has reports whether there has been one.
	last Report
	has  bool
	// partial holds a trailing fragment that may be the start of a sequence.
	partial []byte
}

// Take returns the most recent report and clears it, reporting whether there was one.
//
// Drained rather than read, because a report is an event: the caller forwards it once. Leaving it in place
// would mean re-forwarding the same report on every subsequent chunk of output.
func (t *ReportTracker) Take() (Report, bool) {
	if !t.has {
		return Report{}, false
	}
	r := t.last
	t.last, t.has = Report{}, false
	return r, true
}

// Feed consumes a chunk of shell output and reports whether a report was found.
//
// A last-one-wins collapse when several arrive in one chunk. They describe the same shell, so the newest
// is the truth and forwarding the intermediate ones would only publish states that were already stale.
func (t *ReportTracker) Feed(p []byte) bool {
	// The overwhelmingly common case is output containing no report at all, so it stays cheap. The prefix
	// check is the part that is easy to omit and wrong to omit: a chunk ending mid-introducer holds no
	// complete introducer, and discarding it here makes the report unrecognizable once the rest arrives.
	if len(t.partial) == 0 &&
		!bytes.Contains(p, []byte(reportIntro)) &&
		reportPrefixLen(p) == 0 {
		return false
	}

	buf := p
	if len(t.partial) > 0 {
		buf = append(t.partial, p...)
		t.partial = nil
	}

	found := false
	for {
		i := bytes.Index(buf, []byte(reportIntro))
		if i < 0 {
			t.holdBack(buf)
			break
		}

		tail := buf[i:]
		end, termLen := oscEnd(tail)
		if end < 0 {
			// Unterminated: the parameters are still arriving.
			t.holdBack(tail)
			break
		}

		if r, ok := parseReport(tail[len(reportIntro):end]); ok {
			t.last, t.has = r, true
			found = true
		}
		buf = tail[end+termLen:]
	}

	return found
}

// holdBack retains the part of buf that could be the start of a report.
func (t *ReportTracker) holdBack(buf []byte) {
	keep := reportPrefixLen(buf)
	if keep == 0 {
		return
	}
	if keep > maxPartial {
		// Longer than any real report, so this was not one. Dropping it rather than growing without bound
		// loses nothing, matching CommandTracker.
		return
	}
	t.partial = append([]byte(nil), buf[len(buf)-keep:]...)
}

// reportPrefixLen returns how many trailing bytes of buf could begin a report.
func reportPrefixLen(buf []byte) int {
	if i := bytes.LastIndex(buf, []byte(reportIntro)); i >= 0 {
		return len(buf) - i
	}
	for n := min(len(reportIntro)-1, len(buf)); n > 0; n-- {
		if bytes.HasSuffix(buf, []byte(reportIntro[:n])) {
			return n
		}
	}
	return 0
}

// parseReport reads a report's parameters, which is everything between the introducer and the terminator.
//
// Shaped as key=value fields rather than positionally, so a later version can add one without the
// ordering becoming load-bearing. An unknown key is ignored for the same reason: an old cm meeting a
// newer integration should drop what it does not understand rather than reject the whole report.
//
// Reports whether anything usable was found, so a malformed sequence leaves the previous state alone
// instead of clearing it. A shell emitting nonsense should not be able to erase a valid report.
func parseReport(params []byte) (Report, bool) {
	var r Report
	for _, field := range splitUnescaped(string(params), ';') {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "state":
			// Unescaped like every other value, so a state is read the same way regardless of which
			// field it is. It cannot contain a semicolon in practice, and treating one field specially
			// is how an inconsistency becomes a bug later.
			v := unescapeCmdline(value)
			if reportStates[v] {
				r.State = v
			}
		case "detail":
			r.Detail = unescapeCmdline(value)
		case "source":
			r.Source = unescapeCmdline(value)
		}
	}
	// A report with no valid state says nothing, whatever else it carried.
	return r, r.State != ""
}
