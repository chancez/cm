package osc

import (
	"bytes"
	"strconv"
	"strings"
)

// maxPartial bounds how many trailing bytes a CommandTracker retains while waiting for an
// unterminated escape sequence to finish.
//
// A bound is necessary rather than tidy. The tracker holds back what looks like the start of a
// sequence so it can be matched once the rest arrives, and a stream that emits an ESC and then
// megabytes of ordinary text would otherwise grow that buffer without limit. 4 KiB is far more than
// any real OSC 133 sequence needs, including a long cmdline.
const maxPartial = 4 << 10

// CommandState is what a shell has most recently reported about itself via OSC 133.
type CommandState struct {
	// Running reports whether a command is executing, as opposed to the shell sitting at a prompt.
	Running bool
	// Command is the command line the shell reported, empty when it reported none.
	//
	// Best effort by design: the cmdline parameter is an extension that zsh and bash send under
	// kitty's shell integration and other shells omit. Running is reliable without it, since it
	// depends only on which marker arrived.
	Command string
	// ExitCode is the status of the last finished command, and Exited reports whether there was one.
	//
	// Separate fields because zero is a real exit status and cannot double as "nothing has finished
	// yet".
	ExitCode int
	Exited   bool
	// Runs counts the commands the shell has reported starting.
	//
	// Necessary because Running alone cannot express "a command ran and finished": both that and "nothing
	// has happened" look idle. A caller waiting for a command it just triggered needs to tell them apart,
	// and a fast command's start and end can arrive in the same chunk of output, so there is no moment at
	// which Running is observably true.
	//
	// Counted here rather than by a consumer for exactly that reason: the tracker sees every marker,
	// while a consumer only ever sees the state left behind.
	Runs uint64
}

// CommandTracker follows a shell's OSC 133 reports across a stream of output.
//
// Stateful because the markers are events rather than values: "a command started" only means
// something relative to what came before, and the sequences carrying them can be split across reads.
//
// Not safe for concurrent use. In cm only the output pump feeds one, which is also the only writer to
// terminal state, so no locking is needed.
type CommandTracker struct {
	state CommandState
	// partial holds a trailing fragment that may be the start of a sequence.
	partial []byte
	// seen records whether any marker has been applied, which is not the same question as whether the
	// state is non-zero.
	//
	// Needed because Feed's return value cannot answer it: an A marker arriving at a prompt leaves
	// Running false and Command empty, so it changes nothing and Feed reports no change. A caller that
	// has to tell "the shell is at a prompt" from "this shell reports no OSC 133 at all" -- adoption
	// does, since the two states look identical and only one of them means a report about the session
	// is stale -- has nothing else to read.
	seen bool
}

// State returns what the shell has reported so far.
func (t *CommandTracker) State() CommandState { return t.state }

// Seen reports whether any OSC 133 marker has been applied.
//
// False means nothing has been observed, which happens both for a shell with no integration loaded and
// for output whose markers scrolled out of whatever window the caller fed. Neither is "the shell is
// idle": that is Seen with State().Running false.
func (t *CommandTracker) Seen() bool { return t.seen }

// Feed consumes a chunk of shell output and reports whether the state changed.
//
// Changes are reported rather than assumed so a caller can avoid publishing an update per chunk: a
// shell producing output emits no markers at all, which is the overwhelmingly common case.
func (t *CommandTracker) Feed(p []byte) bool {
	// The common case, and worth keeping cheap: output that contains neither a marker nor the start of
	// one, with nothing held back from a previous call, cannot change anything.
	//
	// The prefix check is the part that is easy to omit and wrong to omit. A chunk ending mid-introducer
	// (a bare ESC, or "\x1b]13") contains no complete introducer, so returning early here discards it,
	// and the marker is then unrecognizable once its remainder arrives. Splits inside the introducer are
	// exactly as likely as splits inside the parameters.
	if len(t.partial) == 0 &&
		!bytes.Contains(p, []byte(commandIntro)) &&
		partialPrefixLen(p) == 0 {
		return false
	}

	buf := p
	if len(t.partial) > 0 {
		// Rejoin what was held back. A sequence split across two reads is not unusual: a pty read is
		// bounded by the kernel buffer, not by anything the shell intends.
		buf = append(t.partial, p...)
		t.partial = nil
	}

	before := t.state
	for {
		i := bytes.Index(buf, []byte(commandIntro))
		if i < 0 {
			// Retain a trailing fragment that could be the beginning of an introducer, so the next
			// call can match it once the rest arrives.
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

		t.apply(tail[len(commandIntro):end])
		buf = tail[end+termLen:]
	}

	return t.state != before
}

// holdBack retains the part of buf that could be the start of an OSC 133 sequence.
func (t *CommandTracker) holdBack(buf []byte) {
	// Only a fragment ending in a possible prefix is worth keeping. Anything else cannot become a
	// sequence however much follows it.
	keep := partialPrefixLen(buf)
	if keep == 0 {
		return
	}
	if keep > maxPartial {
		// Longer than any real sequence, so this is not one. Dropping it rather than growing without
		// bound loses nothing: a marker that never terminated was never a marker.
		return
	}
	t.partial = append([]byte(nil), buf[len(buf)-keep:]...)
}

// partialPrefixLen returns how many trailing bytes of buf could begin an OSC 133 sequence.
//
// Two cases. Either the tail is a prefix of the introducer itself, so the rest of "\x1b]133;" is yet
// to arrive; or the introducer is complete and the parameters are still coming, in which case
// everything from it onward is kept.
func partialPrefixLen(buf []byte) int {
	if i := bytes.LastIndex(buf, []byte(commandIntro)); i >= 0 {
		return len(buf) - i
	}
	for n := min(len(commandIntro)-1, len(buf)); n > 0; n-- {
		if bytes.HasSuffix(buf, []byte(commandIntro[:n])) {
			return n
		}
	}
	return 0
}

// apply updates the state from one sequence's parameters, which is everything after "\x1b]133;" and
// before the terminator.
func (t *CommandTracker) apply(params []byte) {
	if len(params) == 0 {
		return
	}

	// The marker is the first character; anything after it is semicolon-separated parameters.
	switch params[0] {
	case 'A', 'B', 'C', 'D':
		// Recorded before the marker is handled, so it covers the ones that change nothing. An
		// unrecognized marker deliberately does not count: it says a shell integration is loaded but
		// nothing about where the shell is.
		t.seen = true
	}

	switch params[0] {
	case 'A', 'B':
		// A is prompt start and B is prompt end. Either means the shell is at a prompt, so whatever
		// was running has finished, even if its D marker was lost. Being tolerant here matters: a
		// shell interrupted mid-command may print a new prompt without reporting the old command's
		// end, and a session stuck as "busy" forever would be worse than a missed exit status.
		t.state.Running = false
		t.state.Command = ""

	case 'C':
		if !t.state.Running {
			// Counted on the transition, so a shell repeating the marker does not look like a second
			// command.
			t.state.Runs++
		}
		t.state.Running = true
		t.state.Command = commandLine(params)

	case 'D':
		t.state.Running = false
		t.state.Command = ""
		if code, ok := exitStatus(params); ok {
			t.state.ExitCode = code
			t.state.Exited = true
		}
	}
}

// commandLine extracts the cmdline parameter from a 133;C sequence, if it has one.
//
// Splits on unescaped semicolons only. Splitting on every semicolon first, which is the obvious way to
// read a parameter list, truncates any command containing one: `echo a\;b` arrives as `cmdline=echo
// a\` plus a bogus `b` field, and the value handed to a user is silently cut short. The escaping
// exists precisely so a semicolon can appear in a value.
func commandLine(params []byte) string {
	for _, field := range splitUnescaped(string(params), ';') {
		value, ok := strings.CutPrefix(field, "cmdline=")
		if !ok {
			continue
		}
		return unescapeCmdline(value)
	}
	return ""
}

// splitUnescaped splits s on sep, ignoring separators preceded by a backslash.
func splitUnescaped(s string, sep byte) []string {
	var (
		fields []string
		cur    strings.Builder
	)
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\\' && i+1 < len(s):
			// Keep the escape, so the value can be unescaped afterwards as a whole.
			cur.WriteByte(s[i])
			i++
			cur.WriteByte(s[i])
		case s[i] == sep:
			fields = append(fields, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(s[i])
		}
	}
	return append(fields, cur.String())
}

// unescapeCmdline undoes the backslash escaping shells apply to a reported command line.
//
// Necessary because the value is embedded in a semicolon-separated parameter list, so a shell that
// reports `sleep 1` sends `sleep\ 1`. Leaving the backslashes in would put them in front of users in
// `cm list`.
func unescapeCmdline(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			i++
			b.WriteByte(s[i])
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// exitStatus extracts the status from a 133;D sequence, reporting whether it had one.
//
// The parameter is optional: a shell may report that a command ended without saying how.
func exitStatus(params []byte) (int, bool) {
	fields := splitUnescaped(string(params), ';')
	if len(fields) < 2 {
		return 0, false
	}
	// The status is the first positional parameter. Later fields, and any key=value extensions, are
	// not statuses.
	code, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil {
		return 0, false
	}
	return code, true
}

// commandIntro is the OSC 133 introducer, shared by every marker.
const commandIntro = "\x1b]133;"
