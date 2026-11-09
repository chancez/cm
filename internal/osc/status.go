package osc

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"strings"
	"unicode/utf8"
)

// StatusNumber is the OSC number of the program status protocol, which a program uses to say what it is
// doing: idle, working, done, blocked on the user, or failed.
//
// Not cm's own sequence, unlike ReportNumber. The specification is at
// https://www.superlogical.com/rex/docs/build/program-status, ghostty implements it, and agents and build
// tools emit it, so cm reads it from the stream in the same pass as its own and both become the session's
// reported state.
const StatusNumber = 7501

// statusIntro is the introducer every program status sequence begins with.
const statusIntro = "\x1b]7501;"

// The limits the specification sets. A report over any of them is discarded whole rather than truncated,
// which is what the specification requires and what libghostty does; TestStatusParserAgreesWithEmulator
// in internal/vt holds the two to the same answers.
const (
	maxStatusSequence = 4096
	maxStatusKey      = 16
	maxStatusApp      = 32
	maxStatusID       = 128
	maxStatusIDSeg    = 32
	maxStatusIDDepth  = 8
	maxStatusTitleEnc = 256
	maxStatusTitleDec = 192
	maxStatusMsgEnc   = 2732
	maxStatusMsgDec   = 2048
)

// maxPendingStatus bounds how many reports one drain can carry.
//
// The specification's own record limit, so a burst can carry one report per record a terminal must keep.
// Bounded at all because anything that prints is a source, the reason maxPendingNesting gives.
const maxPendingStatus = 256

// Status is one program status report (OSC 7501).
//
// Already validated against the specification: every limit, the character sets, the base64, and the ban
// on control characters in decoded text. A field the report did not carry is empty, and Progress is -1.
type Status struct {
	// State is "idle", "working", "done", "blocked", "error", or "clear", which removes a record and every
	// record beneath it.
	State string
	// Kind says what a blocked program needs: "permission", "question", or "auth". Only set with
	// "blocked", and empty when the program did not say or said something this build does not know.
	Kind string
	// Progress is a percentage from 0 to 100, or -1 for none. Only set with "working" or "blocked".
	Progress int
	// ID names the record the report is about, empty for the program itself. A "/" makes one record the
	// child of another, so "build/test" sits beneath "build".
	ID string
	// App is a stable identifier for the program, such as "cargo", for a machine to match on.
	App string
	// Title is a short label for people, decoded from base64.
	Title string
	// Message is one line for people saying what the record is doing, decoded from base64.
	Message string
}

// statusStates is the set a report may carry. An unknown state makes the whole report meaningless, so it
// is ignored rather than guessed at.
var statusStates = map[string]bool{
	"idle":    true,
	"working": true,
	"done":    true,
	"blocked": true,
	"error":   true,
	"clear":   true,
}

// statusKinds is the set a blocked report may name. An unknown kind is treated as absent, which is what
// the specification says, so a newer program's kind degrades to "blocked" rather than losing the report.
var statusKinds = map[string]bool{
	"permission": true,
	"question":   true,
	"auth":       true,
}

// StatusEvent is one thing a StatusTracker found, in the order the stream carried it.
type StatusEvent struct {
	// Prompt is a shell prompt starting (OSC 133;A), which ends working and blocked records. Status is
	// empty when it is set.
	Prompt bool
	// Status is the report, when Prompt is not set.
	Status Status
}

// StatusTracker follows program status reports across a stream of output, together with the prompts that
// end them.
//
// Stateful for the reason ReportTracker is, and queued rather than last-one-wins: a report is about one
// record of possibly many, so two reports in one chunk can describe two different things, and collapsing
// them would drop one.
//
// A full reset (RIS) is deliberately not a clear, though the specification says it removes every record and
// libghostty reports it as one. cm's own client writes RIS when it detaches, to put its terminal back, and a
// client attached inside a session has that session's pty as its terminal. So every nested detach reset the
// parent: its report vanished the moment the child left, after the nesting had ended and its bytes were the
// parent's again. Fixing the order in the client would not reach an older cm across an ssh. What ignoring it
// costs is a done or error record surviving a `reset`; working and blocked end at the next prompt anyway.
//
// Prompts are found here rather than taken from a CommandTracker because the order between a report and a
// prompt decides what survives, and two parsers cannot say which came first in one chunk. Recovering a
// session after a restart reads its log in large chunks, so "working, prompt, working" in one chunk is the
// normal case there: applying the prompt last ended the second report too.
//
// Not safe for concurrent use. In cm only the output pump feeds one.
type StatusTracker struct {
	pending []StatusEvent
	partial []byte
}

// Feed consumes a chunk of output and reports whether it completed any event.
func (t *StatusTracker) Feed(p []byte) bool {
	// The overwhelmingly common chunk holds no OSC, and checking for exactly that is one scan where checking
	// for each introducer was several: 29ns per 1022-byte read against 2.25us. Every introducer begins with
	// ESC, so a chunk ending in one may be holding the start of the next.
	if len(t.partial) == 0 &&
		!bytes.Contains(p, []byte("\x1b]")) &&
		(len(p) == 0 || p[len(p)-1] != 0x1b) {
		return false
	}

	buf := p
	if len(t.partial) > 0 {
		buf = append(t.partial, p...)
		t.partial = nil
	}

	found := false
	for {
		i, what := t.next(buf)
		if i < 0 {
			t.holdBack(buf)
			break
		}
		tail := buf[i:]
		if what == promptStart {
			// A prompt when followed by a parameter separator or a terminator, so "133;AB" is not one.
			if len(tail) == len(promptStart) {
				// The byte that decides whether this is a prompt has not arrived.
				t.holdTail(tail)
				return found
			}
			if c := tail[len(promptStart)]; c == ';' || c == 0x07 || c == 0x1b {
				t.queue(StatusEvent{Prompt: true})
				found = true
			}
			buf = tail[len(promptStart):]
			continue
		}
		end, termLen := oscEnd(tail)
		if end < 0 {
			// Unterminated: the parameters are still arriving.
			t.holdTail(tail)
			break
		}
		if end+termLen <= maxStatusSequence {
			if s, ok := ParseStatus(tail[len(statusIntro):end]); ok {
				t.queue(StatusEvent{Status: s})
				found = true
			}
		}
		buf = tail[end+termLen:]
	}
	return found
}

// next returns where the earliest event in buf begins and which kind it is, or -1. One walk over the ESC
// bytes rather than a search per introducer, each of which would rescan the rest of the chunk.
func (t *StatusTracker) next(buf []byte) (int, string) {
	for off := 0; ; {
		i := bytes.IndexByte(buf[off:], 0x1b)
		if i < 0 {
			return -1, ""
		}
		i += off
		for _, intro := range [...]string{statusIntro, promptStart} {
			if bytes.HasPrefix(buf[i:], []byte(intro)) {
				return i, intro
			}
		}
		off = i + 1
	}
}

// queue appends an event, dropping it once a drain already holds the limit.
func (t *StatusTracker) queue(e StatusEvent) {
	if len(t.pending) < maxPendingStatus {
		t.pending = append(t.pending, e)
	}
}

// Take returns the events since the last call, oldest first, and clears them.
//
// In order because a later report replaces an earlier one about the same record, a clear removes what
// came before it, and a prompt ends only what was reported before it.
func (t *StatusTracker) Take() []StatusEvent {
	if len(t.pending) == 0 {
		return nil
	}
	out := t.pending
	t.pending = nil
	return out
}

// holdBack retains the end of a chunk in which next found nothing, as far as it could be the start of an
// event. Only a suffix check: next already ruled out a whole introducer anywhere in buf. Searching for one
// again from the end, as ReportTracker does, measured 2.3us per 1022-byte read carrying a prompt; this is
// 86ns.
func (t *StatusTracker) holdBack(buf []byte) {
	keep := 0
	for _, intro := range [...]string{statusIntro, promptStart} {
		for n := min(len(intro)-1, len(buf)); n > keep; n-- {
			if bytes.HasSuffix(buf, []byte(intro[:n])) {
				keep = n
				break
			}
		}
	}
	if keep > 0 {
		t.partial = append([]byte(nil), buf[len(buf)-keep:]...)
	}
}

// holdTail retains an event that began but has not finished arriving.
func (t *StatusTracker) holdTail(tail []byte) {
	if len(tail) > maxPartial {
		// Longer than the specification lets a report be, so this was not one.
		return
	}
	t.partial = append([]byte(nil), tail...)
}

// introPrefixLen returns how many trailing bytes of buf could begin a sequence with the given introducer:
// everything from its last complete occurrence, or the longest suffix that is a prefix of it.
func introPrefixLen(buf []byte, intro string) int {
	if i := bytes.LastIndex(buf, []byte(intro)); i >= 0 {
		return len(buf) - i
	}
	for n := min(len(intro)-1, len(buf)); n > 0; n-- {
		if bytes.HasSuffix(buf, []byte(intro[:n])) {
			return n
		}
	}
	return 0
}

// ParseStatus reads a report's payload, everything between the introducer and the terminator, reporting
// whether it was a valid report.
//
// The support query, "?", is not a report and is rejected here; the emulator answers it.
func ParseStatus(payload []byte) (Status, bool) {
	s := Status{Progress: -1}
	var kind, progress, title, msg string
	var hasID bool
	for _, pair := range strings.Split(string(payload), ":") {
		key, value, ok := strings.Cut(pair, "=")
		// Over the key limit discards the report, where a malformed pair is only skipped. The distinction
		// is libghostty's, found by the agreement test: a limit is a limit, a typo is not.
		if ok && len(key) > maxStatusKey {
			return Status{}, false
		}
		// A malformed pair is skipped rather than failing the report, as the specification says.
		if !ok || !validStatusKey(key) || !validStatusValue(value) {
			continue
		}
		// Repeated keys: the last one wins.
		switch key {
		case "state":
			s.State = value
		case "id":
			s.ID, hasID = value, true
		case "kind":
			kind = value
		case "progress":
			progress = value
		case "app":
			s.App = value
		case "title":
			title = value
		case "msg":
			msg = value
		}
	}
	if !statusStates[s.State] {
		return Status{}, false
	}
	if hasID && !validStatusID(s.ID) {
		return Status{}, false
	}
	// An app over the limit discards the report, while one with the wrong characters, or none at all, is
	// only absent. libghostty's split again, and it matters more than it looks: "a/b" is a valid value
	// and an invalid app, so a program reporting an app shaped like a path still reports its state.
	if len(s.App) > maxStatusApp {
		return Status{}, false
	}
	if !validStatusApp(s.App) {
		s.App = ""
	}

	s.Kind = kind
	if n, err := strconv.Atoi(progress); err == nil && validDigits(progress) {
		s.Progress = n
	}
	s = s.Normalized()

	var ok bool
	if s.Title, ok = decodeStatusText(title, maxStatusTitleEnc, maxStatusTitleDec); !ok {
		return Status{}, false
	}
	if s.Message, ok = decodeStatusText(msg, maxStatusMsgEnc, maxStatusMsgDec); !ok {
		return Status{}, false
	}
	return s, true
}

// Normalized applies the rules for which fields a state may carry: a kind only with "blocked" and only one
// this build knows, and a progress only with "working" or "blocked" and only from 0 to 100. Anything else
// is absent, as the specification says, rather than an error.
//
// A method so a report that did not come from the wire, such as `cm report`, follows the same rules.
func (s Status) Normalized() Status {
	if s.State != "blocked" || !statusKinds[s.Kind] {
		s.Kind = ""
	}
	if (s.State != "working" && s.State != "blocked") || s.Progress < 0 || s.Progress > 100 {
		s.Progress = -1
	}
	return s
}

// decodeStatusText decodes a title or message, reporting false for anything the specification says
// discards the whole report: too long encoded or decoded, invalid base64, or a control character.
func decodeStatusText(enc string, maxEnc, maxDec int) (string, bool) {
	if enc == "" {
		return "", true
	}
	if len(enc) > maxEnc {
		return "", false
	}
	// Padding is optional, so it is stripped and the unpadded alphabet decodes either spelling.
	dec, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(enc, "="))
	if err != nil || len(dec) > maxDec || !utf8.Valid(dec) {
		return "", false
	}
	text := string(dec)
	for _, r := range text {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return "", false
		}
	}
	return text, true
}

// validStatusKey reports whether a key is lowercase letters within the length limit.
func validStatusKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 'a' || key[i] > 'z' {
			return false
		}
	}
	return true
}

// validStatusValue reports whether a value uses only the characters the specification allows, which is
// base64 plus the separators an id or app may contain.
func validStatusValue(v string) bool {
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == ',', c == '+', c == '/', c == '=', c == '-':
		default:
			return false
		}
	}
	return true
}

// validStatusName reports whether s is a nonempty run of [A-Za-z0-9_.+-] no longer than limit, which is
// the shape of an app and of each segment of an id.
func validStatusName(s string, limit int) bool {
	if s == "" || len(s) > limit {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '.', c == '+', c == '-':
		default:
			return false
		}
	}
	return true
}

// validStatusApp reports whether app has the shape the specification allows. Length is checked by the
// caller, since over the limit discards the report and the wrong shape does not.
func validStatusApp(app string) bool { return validStatusName(app, maxStatusApp) }

func validStatusID(id string) bool {
	if len(id) > maxStatusID {
		return false
	}
	segs := strings.Split(id, "/")
	if len(segs) > maxStatusIDDepth {
		return false
	}
	for _, seg := range segs {
		if !validStatusName(seg, maxStatusIDSeg) {
			return false
		}
	}
	return true
}

// validDigits reports whether s is only ASCII digits, since strconv.Atoi also accepts a sign.
func validDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return s != ""
}

// StatusSequence returns the bytes that report s, BEL terminated.
//
// Beside the parser for the reason FrameSequence is: the shell integrations build the same bytes with
// printf, and internal/shellinit compares them against this.
func StatusSequence(s Status) []byte {
	var b strings.Builder
	b.WriteString(statusIntro)
	b.WriteString("state=" + s.State)
	if s.ID != "" {
		b.WriteString(":id=" + s.ID)
	}
	if s.Kind != "" {
		b.WriteString(":kind=" + s.Kind)
	}
	if s.Progress >= 0 {
		b.WriteString(":progress=" + strconv.Itoa(s.Progress))
	}
	if s.App != "" {
		b.WriteString(":app=" + s.App)
	}
	if s.Title != "" {
		b.WriteString(":title=" + base64.StdEncoding.EncodeToString([]byte(s.Title)))
	}
	if s.Message != "" {
		b.WriteString(":msg=" + base64.StdEncoding.EncodeToString([]byte(s.Message)))
	}
	b.WriteString("\a")
	return []byte(b.String())
}
