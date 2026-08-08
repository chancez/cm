package osc

import (
	"strings"
	"testing"
)

// The sequences a real shell sends, captured from zsh under kitty's shell integration on a pty rather
// than written from the specification.
//
// This matters more than it looks. The cmdline extension is not in every shell, the escaping of spaces
// inside it is easy to guess wrong, and a test built from what the spec permits can pass while failing
// on what shells actually emit. Captured: 133;A, 133;C;cmdline=sleep\ 1, 133;D;0.
const (
	realPromptStart = "\x1b]133;A\x07"
	realCommandRun  = "\x1b]133;C;cmdline=sleep\\ 1\x07"
	realCommandDone = "\x1b]133;D;0\x07"
)

// The whole point: a session at a prompt is idle, and one running a command is busy.
//
// This is what lets a terminal emulator ask "really close this?" only when something is running. zmx
// needed shell hooks maintaining a label for it; the shell already reports it.
func TestCommandTrackerFollowsARealShell(t *testing.T) {
	var tr CommandTracker

	if got := tr.State(); got != (CommandState{}) {
		t.Errorf("initial State() = %+v, want the zero state", got)
	}

	// At a prompt, nothing running.
	if changed := tr.Feed([]byte(realPromptStart)); changed {
		t.Error("Feed(prompt start) reported a change from the zero state, want none")
	}
	if got := tr.State(); got.Running {
		t.Errorf("State() = %+v after a prompt, want Running false", got)
	}

	// A command starts.
	if changed := tr.Feed([]byte(realCommandRun)); !changed {
		t.Error("Feed(command start) reported no change, want one")
	}
	want := CommandState{Running: true, Command: "sleep 1", Runs: 1}
	if got := tr.State(); got != want {
		t.Errorf("State() = %+v, want %+v", got, want)
	}

	// And finishes, with a status. Runs stays at 1: it counts commands started, so it does not go back
	// down, which is what lets a caller see that one ran.
	if changed := tr.Feed([]byte(realCommandDone)); !changed {
		t.Error("Feed(command done) reported no change, want one")
	}
	want = CommandState{Running: false, Command: "", ExitCode: 0, Exited: true, Runs: 1}
	if got := tr.State(); got != want {
		t.Errorf("State() = %+v, want %+v", got, want)
	}
}

// The reported command line must be readable, not backslash-escaped.
//
// The value travels inside a semicolon-separated parameter list, so a shell escapes spaces in it: the
// captured sequence for `sleep 1` is `cmdline=sleep\ 1`. Passing that through unchanged would put
// backslashes in front of the user in `cm list`.
func TestCommandTrackerUnescapesTheCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		seq  string
		want string
	}{
		{
			name: "escaped space",
			seq:  "\x1b]133;C;cmdline=sleep\\ 1\x07",
			want: "sleep 1",
		},
		{
			name: "escaped semicolon, which would otherwise split the parameters",
			seq:  "\x1b]133;C;cmdline=echo\\ a\\;b\x07",
			want: "echo a;b",
		},
		{
			name: "escaped backslash",
			seq:  "\x1b]133;C;cmdline=grep\\ \\\\d\x07",
			want: `grep \d`,
		},
		{
			name: "nothing to unescape",
			seq:  "\x1b]133;C;cmdline=make\x07",
			want: "make",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr CommandTracker
			tr.Feed([]byte(tc.seq))
			if got := tr.State().Command; got != tc.want {
				t.Errorf("Command = %q, want %q", got, tc.want)
			}
		})
	}
}

// A shell that reports no cmdline must still be known to be busy.
//
// The extension is what zsh and bash send under kitty's shell integration; other shells send a bare
// 133;C. Running has to work without it, since it depends only on which marker arrived.
func TestCommandTrackerRunningWithoutACommandLine(t *testing.T) {
	var tr CommandTracker
	tr.Feed([]byte("\x1b]133;C\x07"))

	want := CommandState{Running: true, Runs: 1}
	if got := tr.State(); got != want {
		t.Errorf("State() = %+v, want %+v", got, want)
	}
}

// A sequence split across two reads must still be recognized.
//
// Not a contrived case: a pty read is bounded by the kernel buffer rather than by anything the shell
// intends, so any sequence can arrive in pieces. Missing one would leave a session's state wrong until
// the next marker, which for a long-running command is the whole time it matters.
func TestCommandTrackerHandlesSplitSequences(t *testing.T) {
	// Every split point, since the interesting ones are inside the introducer and inside the
	// parameters, and picking a few by hand would miss the boundary cases.
	seq := realCommandRun
	for cut := 1; cut < len(seq); cut++ {
		var tr CommandTracker
		tr.Feed([]byte(seq[:cut]))
		tr.Feed([]byte(seq[cut:]))

		want := CommandState{Running: true, Command: "sleep 1", Runs: 1}
		if got := tr.State(); got != want {
			t.Errorf("split at %d: State() = %+v, want %+v", cut, got, want)
		}
	}
}

// Markers mixed into ordinary output must be found, and the output itself ignored.
func TestCommandTrackerFindsMarkersAmongOutput(t *testing.T) {
	var tr CommandTracker
	tr.Feed([]byte("some output\r\n" + realCommandRun + "more output\r\n"))

	if got := tr.State(); !got.Running || got.Command != "sleep 1" {
		t.Errorf("State() = %+v, want a running sleep 1", got)
	}

	tr.Feed([]byte("still running\r\n"))
	if got := tr.State(); !got.Running {
		t.Errorf("State() = %+v after plain output, want it still running", got)
	}
}

// A new prompt must clear a running command even without an end marker.
//
// This is the tolerance that keeps a session from being stuck as "busy" forever. A command interrupted
// with ctrl-c, or a shell that loses its D marker, still prints a new prompt, and treating that as
// "back at a prompt" is both true and the only recovery available. Reporting a session as busy
// indefinitely would make a close confirmation useless, since it would always fire.
func TestCommandTrackerPromptClearsAStuckCommand(t *testing.T) {
	var tr CommandTracker
	tr.Feed([]byte(realCommandRun))
	if !tr.State().Running {
		t.Fatal("setup: want a running command")
	}

	// No D marker, straight to a new prompt.
	if changed := tr.Feed([]byte(realPromptStart)); !changed {
		t.Error("a new prompt after a lost end marker reported no change, want one")
	}
	if got := tr.State(); got.Running || got.Command != "" {
		t.Errorf("State() = %+v after a new prompt, want idle with no command", got)
	}
}

// A non-zero exit status must be preserved, and distinguishable from "nothing has finished".
func TestCommandTrackerExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seq      string
		wantCode int
		wantHave bool
	}{
		{name: "success", seq: "\x1b]133;D;0\x07", wantCode: 0, wantHave: true},
		{name: "failure", seq: "\x1b]133;D;1\x07", wantCode: 1, wantHave: true},
		{name: "signal-ish", seq: "\x1b]133;D;130\x07", wantCode: 130, wantHave: true},
		// A shell may report that a command ended without saying how, and 0 must not be invented for
		// it: that would report a failed command as successful.
		{name: "no status given", seq: "\x1b]133;D\x07", wantCode: 0, wantHave: false},
		{name: "unparseable status", seq: "\x1b]133;D;oops\x07", wantCode: 0, wantHave: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr CommandTracker
			tr.Feed([]byte(realCommandRun))
			tr.Feed([]byte(tc.seq))

			st := tr.State()
			if st.Running {
				t.Error("still running after an end marker")
			}
			if st.Exited != tc.wantHave {
				t.Errorf("Exited = %v, want %v", st.Exited, tc.wantHave)
			}
			if st.Exited && st.ExitCode != tc.wantCode {
				t.Errorf("ExitCode = %d, want %d", st.ExitCode, tc.wantCode)
			}
		})
	}
}

// Output with no markers must not report a change, since the caller publishes an update when one does.
//
// A shell producing output emits no markers at all, which is the overwhelmingly common case: reporting
// a change per chunk would wake every subscribed client for every line of output.
func TestCommandTrackerReportsNoChangeForPlainOutput(t *testing.T) {
	var tr CommandTracker
	if changed := tr.Feed([]byte("just some output\r\n")); changed {
		t.Error("Feed(plain output) reported a change, want none")
	}

	tr.Feed([]byte(realCommandRun))
	if changed := tr.Feed([]byte("output from the command\r\n")); changed {
		t.Error("Feed(output while running) reported a change, want none")
	}
	// Repeating a marker that changes nothing is also not a change.
	if changed := tr.Feed([]byte(realCommandRun)); changed {
		t.Error("Feed(the same command start again) reported a change, want none")
	}
}

// A stream that emits an introducer and then never terminates it must not grow memory without bound.
//
// The tracker holds back what might be an unfinished sequence, so an unterminated one is exactly the
// case where that buffer could grow forever. A shell will not do this; a program writing raw bytes
// might, and cm passes everything through.
func TestCommandTrackerBoundsAnUnterminatedSequence(t *testing.T) {
	var tr CommandTracker
	tr.Feed([]byte("\x1b]133;C;cmdline="))
	tr.Feed([]byte(strings.Repeat("x", maxPartial*4)))

	if len(tr.partial) > maxPartial {
		t.Errorf("retained %d bytes waiting for a terminator, want at most %d",
			len(tr.partial), maxPartial)
	}
	// And the tracker still works afterwards rather than being wedged.
	tr.Feed([]byte(realPromptStart))
	tr.Feed([]byte(realCommandRun))
	if got := tr.State(); !got.Running {
		t.Errorf("State() = %+v after recovering, want a running command", got)
	}
}

// Both OSC terminators have to work, since shells use both.
func TestCommandTrackerAcceptsBothTerminators(t *testing.T) {
	for _, tc := range []struct{ name, seq string }{
		{name: "BEL", seq: "\x1b]133;C;cmdline=make\x07"},
		{name: "ST", seq: "\x1b]133;C;cmdline=make\x1b\\"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr CommandTracker
			tr.Feed([]byte(tc.seq))
			if got := tr.State(); !got.Running || got.Command != "make" {
				t.Errorf("State() = %+v, want a running make", got)
			}
		})
	}
}

// A command whose start and end arrive together must still be counted.
//
// This is the case Running cannot express and the reason Runs exists. A fast command produces both
// markers in one read, so the tracker's state goes straight from idle to idle: nothing observes it
// running, and a caller waiting for "the command I sent finished" has no evidence it ever started.
//
// Found on Linux, where `true` reliably produced this, while macOS happened to split the chunk and hid
// it. That asymmetry is worth noting: the same code passed on one platform and failed on the other for
// reasons entirely down to read timing.
func TestCommandTrackerCountsACommandThatNeverAppearsRunning(t *testing.T) {
	var tr CommandTracker
	tr.Feed([]byte(realPromptStart))

	before := tr.State().Runs

	// One chunk: command start, its output, its end, and the next prompt.
	tr.Feed([]byte(realCommandRun + "output\r\n" + realCommandDone + realPromptStart))

	st := tr.State()
	if st.Running {
		t.Errorf("State() = %+v, want it back at a prompt", st)
	}
	if st.Runs != before+1 {
		t.Errorf("Runs = %d, want %d: a command ran even though it was never observed running",
			st.Runs, before+1)
	}
}

// Runs must count commands, not markers.
func TestCommandTrackerRunsCountsEachCommandOnce(t *testing.T) {
	var tr CommandTracker

	tr.Feed([]byte(realCommandRun))
	if got := tr.State().Runs; got != 1 {
		t.Fatalf("Runs = %d after one command, want 1", got)
	}
	// A repeated marker for the same command is not a new one.
	tr.Feed([]byte(realCommandRun))
	if got := tr.State().Runs; got != 1 {
		t.Errorf("Runs = %d after a repeated start marker, want 1", got)
	}

	tr.Feed([]byte(realCommandDone))
	if got := tr.State().Runs; got != 1 {
		t.Errorf("Runs = %d after the command ended, want it to stay at 1", got)
	}

	tr.Feed([]byte("\x1b]133;C;cmdline=make\x07"))
	if got := tr.State().Runs; got != 2 {
		t.Errorf("Runs = %d after a second command, want 2", got)
	}
}

// The exit status from 133;D is recorded, and distinguishable from no status at all.
//
// cm parsed this from the start and exposed it nowhere, so a failing command left no trace: running `false`
// in a session looked identical to running `true`. The parsing was already right; these pin it so the
// plumbing above cannot drift.
func TestCommandTrackerRecordsExitStatus(t *testing.T) {
	for _, tc := range []struct {
		name         string
		input        string
		wantExited   bool
		wantExitCode int
	}{
		{
			// What kitty's zsh integration sends: `\e]133;D;<status>\a`.
			name:         "success",
			input:        "\x1b]133;C\x07\x1b]133;D;0\x07",
			wantExited:   true,
			wantExitCode: 0,
		},
		{
			name:         "failure",
			input:        "\x1b]133;C\x07\x1b]133;D;1\x07",
			wantExited:   true,
			wantExitCode: 1,
		},
		{
			name:         "a larger status",
			input:        "\x1b]133;C\x07\x1b]133;D;42\x07",
			wantExited:   true,
			wantExitCode: 42,
		},
		{
			// A bare D with no status, which is legal and which kitty sends when it has none. Exited stays
			// false, because reporting 0 here would claim a success that was never reported.
			name:       "no status parameter",
			input:      "\x1b]133;C\x07\x1b]133;D\x07",
			wantExited: false,
		},
		{
			// A command that has started and not finished: nothing to report yet.
			name:       "still running",
			input:      "\x1b]133;C\x07",
			wantExited: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var tr CommandTracker
			tr.Feed([]byte(tc.input))

			got := tr.State()
			if got.Exited != tc.wantExited {
				t.Errorf("Exited = %v, want %v", got.Exited, tc.wantExited)
			}
			if tc.wantExited && got.ExitCode != tc.wantExitCode {
				t.Errorf("ExitCode = %d, want %d", got.ExitCode, tc.wantExitCode)
			}
		})
	}
}

// A new command clears the previous status, so a stale one is never reported as current.
//
// Without this a caller checking after a successful command could see the failure before it, which is worse
// than no answer: it attributes one command's outcome to another.
func TestCommandTrackerClearsStatusOnANewCommand(t *testing.T) {
	var tr CommandTracker
	tr.Feed([]byte("\x1b]133;C\x07\x1b]133;D;5\x07"))
	if got := tr.State(); !got.Exited || got.ExitCode != 5 {
		t.Fatalf("after a failure: Exited=%v ExitCode=%d, want true and 5", got.Exited, got.ExitCode)
	}

	// A second command starts. While it runs, the previous status must not read as this one's.
	tr.Feed([]byte("\x1b]133;C\x07"))
	if got := tr.State(); !got.Running {
		t.Error("Running = false after a command started")
	}

	tr.Feed([]byte("\x1b]133;D;0\x07"))
	got := tr.State()
	if !got.Exited || got.ExitCode != 0 {
		t.Errorf("after a success: Exited=%v ExitCode=%d, want true and 0", got.Exited, got.ExitCode)
	}
}
