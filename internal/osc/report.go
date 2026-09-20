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

// Nesting is a client saying that it is attached inside the session whose pty this is.
//
// Distinct from a Report, which is what a shell said about itself. This is what a *client* says about
// itself, and it exists because a client cannot always tell the server: `CM_SESSION` does not cross an
// ssh, so a `cm attach` on another host has no parent to name in its Open request. Its stdout is this
// session's pty, so the one channel that does reach the parent is the byte stream.
//
// The parent needs this for the detach key. See Session.beginHosting.
type Nesting struct {
	// ID pairs an end with its begin, and is the announcing client's own nonce rather than a session
	// reference: a remote session's ID means nothing on this host, and two hosts can mint the same one.
	ID string
	// Ended distinguishes a client leaving from a client arriving.
	Ended bool
}

// Frame is a command the session's shell is running, opened at its prompt hook and closed at the next one.
//
// The third thing cm's own sequence carries, alongside a Report and a Nesting. What it is for is stated in
// docs/ideas.md under a session's location: cm has a cwd and a busy flag, both single values derived from
// bytes, and no notion of "this session is inside something, entered by this command". A frame is that
// notion, and the reason it is a sequence rather than a derivation is in the same entry: OSC 133 carries a
// command line that cm has to discard the moment a nested shell prompts, because nothing in it can tell a
// nested shell from a shell that prompted after an interrupt.
//
// It is also what collects a nesting announcement whose client is gone. An announcement binds to the frame
// open when it arrived, and a frame closing discards it, so a dropped ssh is forgotten when the parent
// shell reaches its next prompt rather than standing until the server restarts.
type Frame struct {
	// ID pairs a close with its open, and is minted by the shell that emitted it: one salt per shell plus a
	// counter, so frames from two shells in an ssh chain cannot be mistaken for each other.
	ID string
	// Argv is the command line as the shell reported it, already unescaped. Empty on a close.
	Argv string
	// Ended distinguishes a frame closing from one opening.
	Ended bool
}

// maxFrameArgv bounds how much of a command line is kept.
//
// Long enough for an ssh invocation with options, which is what this exists to record, and short enough
// that a session cannot be made to hold a screenful per command. The shells truncate as well; this is the
// bound that does not depend on them getting it right.
const maxFrameArgv = 256

// maxPendingNesting bounds how many announcements one drain can carry.
//
// Announcements arrive as bytes in a session's output, so anything that prints them is a source: `cat`
// of a file containing one is the honest case. Bounded here and again in the server, which keeps a
// stream of junk from growing either the slice or the parent's map without limit. The consequence of a
// spurious announcement is documented where it is acted on, and is why the prefix key stays with the
// outer client while a nesting is only announced.
const maxPendingNesting = 32

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
	// frames holds command frames not yet drained, in the order they arrived.
	//
	// A queue for the same reason nests is one: an open and its close are not interchangeable, and a
	// command fast enough to produce both in one chunk would otherwise leave a frame that never closes.
	frames []Frame
	// nests holds announcements not yet drained, in the order they arrived.
	//
	// A queue rather than last-one-wins like a report, because these do not describe one changing value:
	// a begin and an end in the same chunk cancel out, and collapsing them would leave the parent
	// believing a client that has already gone is still there.
	nests []Nesting
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

// TakeNesting returns the announcements since the last call, oldest first, and clears them.
//
// Drained like Take and for the same reason: each one is an event the caller applies once. Returning
// them in order matters here, unlike a report, because a begin and its end are not interchangeable.
func (t *ReportTracker) TakeNesting() []Nesting {
	if len(t.nests) == 0 {
		return nil
	}
	out := t.nests
	t.nests = nil
	return out
}

// TakeFrames returns the command frames since the last call, oldest first, and clears them.
//
// Ordered and drained like TakeNesting, and for the same reason: each one moves a stack, so applying them
// out of order or twice leaves the stack describing something that never happened.
func (t *ReportTracker) TakeFrames() []Frame {
	if len(t.frames) == 0 {
		return nil
	}
	out := t.frames
	t.frames = nil
	return out
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

		// The payloads that carry no state come first, since parseReport rejects anything without one and
		// would otherwise discard them as malformed.
		payload := tail[len(reportIntro):end]
		if n, ok := parseNesting(payload); ok {
			if len(t.nests) < maxPendingNesting {
				t.nests = append(t.nests, n)
			}
		} else if f, ok := parseFrame(payload); ok {
			if len(t.frames) < maxPendingNesting {
				t.frames = append(t.frames, f)
			}
		} else if r, ok := parseReport(payload); ok {
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

// NestingSequence returns the bytes a client writes to announce itself to whatever owns its stdout.
//
// Here rather than in the client so the writer and the reader are one file apart: the spelling is a
// protocol between two processes that are usually different builds of cm, since the far side of an ssh
// upgrades on its own schedule. An older parent ignores a payload it cannot parse, which is the
// behaviour parseReport already documents for an unknown key.
//
// BEL terminated, matching what the shell integration emits and what every cm reader accepts.
func NestingSequence(id string, ended bool) []byte {
	state := "begin"
	if ended {
		state = "end"
	}
	return []byte(reportIntro + "client=" + state + ";id=" + id + "\a")
}

// parseNesting reads a nesting announcement, reporting whether the payload was one.
//
// Strict about the id on purpose. The value becomes a key in the parent's map of what is nested inside
// it, and these bytes can come from anything that prints, so a bounded character set keeps a stray
// sequence from putting arbitrary text there. Nonces cm mints are hex, well inside this.
func parseNesting(params []byte) (Nesting, bool) {
	var n Nesting
	var sawClient bool
	for _, field := range splitUnescaped(string(params), ';') {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "client":
			switch unescapeCmdline(value) {
			case "begin":
				sawClient = true
			case "end":
				sawClient, n.Ended = true, true
			}
		case "id":
			n.ID = unescapeCmdline(value)
		}
	}
	if !sawClient || !validNestingID(n.ID) {
		return Nesting{}, false
	}
	return n, true
}

// FrameSequence returns the bytes a shell writes to open or close a command frame.
//
// Here beside the parser for the reason NestingSequence is: the spelling is a contract between cm and a
// shell integration that a user may have loaded from an older build, and one place to read both halves is
// what keeps them agreeing. The shells build this with printf rather than calling into Go, so this is also
// the statement of what they must produce, and internal/shellinit has a test that compares them.
func FrameSequence(id, argv string, ended bool) []byte {
	if ended {
		return []byte(reportIntro + "frame=exit;id=" + id + "\a")
	}
	return []byte(reportIntro + "frame=enter;id=" + id + ";argv=" + escapeCmdline(argv) + "\a")
}

// parseFrame reads a command frame, reporting whether the payload was one.
//
// The id is validated exactly as an announcement's is, and for the same reason: it becomes a key, these
// bytes can come from anything that prints, and a bounded character set is what keeps arbitrary text out.
func parseFrame(params []byte) (Frame, bool) {
	var f Frame
	var sawFrame bool
	for _, field := range splitUnescaped(string(params), ';') {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "frame":
			switch unescapeCmdline(value) {
			case "enter":
				sawFrame = true
			case "exit":
				sawFrame, f.Ended = true, true
			}
		case "id":
			f.ID = unescapeCmdline(value)
		case "argv":
			f.Argv = unescapeCmdline(value)
		}
	}
	if !sawFrame || !validNestingID(f.ID) {
		return Frame{}, false
	}
	// Bounded and stripped here rather than trusting the shells, which truncate too. A control byte in a
	// command line would travel through cm into `cm list --json` and out to whatever reads it, and a
	// newline would break a line-oriented consumer, so neither is carried.
	f.Argv = sanitizeArgv(f.Argv)
	return f, true
}

// sanitizeArgv makes a reported command line safe to carry and bounded in size.
func sanitizeArgv(argv string) string {
	if len(argv) > maxFrameArgv {
		argv = argv[:maxFrameArgv]
	}
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, argv)
}

// escapeCmdline is the inverse of unescapeCmdline, for the one caller that writes a value cm will read
// back: the semicolon separates fields and the backslash escapes, so both have to survive the round trip.
func escapeCmdline(v string) string {
	v = strings.ReplaceAll(v, "\\", "\\\\")
	return strings.ReplaceAll(v, ";", "\\;")
}

// validNestingID reports whether an announced id is safe to use as a key.
func validNestingID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
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
