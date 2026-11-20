package server

import (
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/store"
)

// A declaration lasts as long as the program that made it.
//
// The flaw this guards against is zellij's: a declaration that lasts until the pane closes, so a pane back
// at its shell prompt resurrects into the program that was deliberately quit.
func TestAResumeCommandEndsWithItsFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("sh-1", "claude", false), 0)
	sess.SetResume([]string{"claude", "--resume", "abc"})
	want := store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: "sh-1"}
	if got := sess.Resume(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resume() = %+v, want %+v", got, want)
	}

	sess.processChunk(frame("sh-1", "", true), 0)
	if got := sess.Resume(); !reflect.DeepEqual(got, store.ResumeCommand{}) {
		t.Errorf("Resume() after the program exited = %+v, want the zero value", got)
	}
}

// Closing an outer frame takes a declaration made in a frame above it, the way it takes the frames.
//
// The case: claude running over an ssh declares, the link drops, and the local shell's prompt closes the
// ssh's frame. The remote frame never closes on its own.
func TestAResumeCommandEndsWithAnOuterFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("local-1", "ssh white", false), 0)
	sess.processChunk(frame("remote-1", "claude", false), 0)
	sess.SetResume([]string{"claude", "--resume", "abc"})

	sess.processChunk(frame("local-1", "", true), 0)
	if got := sess.Resume(); !reflect.DeepEqual(got, store.ResumeCommand{}) {
		t.Errorf("Resume() = %+v, want the zero value", got)
	}
}

// A frame other than the declaring one closing leaves the declaration alone.
//
// nvim's :!make opens and closes a frame of its own when the shell integration runs inside it, and that must
// not withdraw nvim's declaration.
func TestAResumeCommandSurvivesAnInnerFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("sh-1", "nvim", false), 0)
	sess.SetResume([]string{"nvim", "-S", "s.vim"})
	sess.processChunk(frame("sub-1", "make", false), 0)
	sess.processChunk(frame("sub-1", "", true), 0)

	want := store.ResumeCommand{Argv: []string{"nvim", "-S", "s.vim"}, Frame: "sh-1"}
	if got := sess.Resume(); !reflect.DeepEqual(got, want) {
		t.Errorf("Resume() = %+v, want %+v", got, want)
	}
}

// A declaration whose frame is not on the stack still ends when that frame closes.
//
// After a server restart the declaration comes back from the store with its frame id, but the stack is
// rebuilt from retained output, and a frame opened long enough ago has been trimmed out of it. The close is
// unmatched as far as the stack is concerned and still means the program exited.
func TestAResumeCommandEndsWithAFrameNotOnTheStack(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sess.resume = store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: "old-7"}

	sess.processChunk(frame("old-7", "", true), 0)
	if got := sess.Resume(); !reflect.DeepEqual(got, store.ResumeCommand{}) {
		t.Errorf("Resume() = %+v, want the zero value", got)
	}
}

// A declaration carried into a revived session binds to the next frame, which is the restored command's.
//
// Unbound, it would outlive the resumed program: quit the resumed claude and the next reboot resumes it
// again. Bound to the old incarnation's frame, nothing would ever close it, with the same result.
func TestARevivedResumeCommandBindsToTheNextFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sess.resume = store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: resumeNextFrame}

	sess.processChunk(frame("new-1", "claude --resume abc", false), 0)
	want := store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: "new-1"}
	if got := sess.Resume(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resume() = %+v, want %+v", got, want)
	}

	sess.processChunk(frame("new-1", "", true), 0)
	if got := sess.Resume(); !reflect.DeepEqual(got, store.ResumeCommand{}) {
		t.Errorf("Resume() after the resumed program exited = %+v, want the zero value", got)
	}
}

// With no frame open the declaration is unbound and no close withdraws it.
//
// That is a shell without cm's integration, where nothing will ever say the program exited, so the
// declaration lasts until it is replaced or cleared.
func TestAResumeCommandWithNoFrameIsUnbound(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.SetResume([]string{"htop"})
	sess.processChunk(frame("x-1", "", true), 0)

	want := store.ResumeCommand{Argv: []string{"htop"}}
	if got := sess.Resume(); !reflect.DeepEqual(got, want) {
		t.Errorf("Resume() = %+v, want %+v", got, want)
	}
	sess.ClearResume()
	if got := sess.Resume(); !reflect.DeepEqual(got, store.ResumeCommand{}) {
		t.Errorf("Resume() after ClearResume = %+v, want the zero value", got)
	}
}

// A session being ended by `cm kill --keep` keeps the declaration it had when the kill arrived.
//
// The hangup is what closes the program's frame: zsh prints "zsh: hangup" and draws a prompt before it dies,
// so without this a suspended session withdrew its own declaration 36ms after the kill and revived into a
// bare shell. A clear or a new declaration from the dying program is held off the same way, since an exit
// hook can send either.
func TestAKillThatKeepsTheSessionKeepsItsResumeCommand(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("sh-1", "claude", false), 0)
	sess.SetResume([]string{"claude", "--resume", "abc"})
	want := store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: "sh-1"}

	sess.keepRecord.Store(true)
	sess.processChunk(frame("sh-1", "", true), 0)
	if got := sess.Resume(); !reflect.DeepEqual(got, want) {
		t.Errorf("Resume() after the frame closed under a keeping kill = %+v, want %+v", got, want)
	}
	sess.ClearResume()
	sess.SetResume([]string{"claude"})
	if got := sess.Resume(); !reflect.DeepEqual(got, want) {
		t.Errorf("Resume() after a clear and a redeclare under a keeping kill = %+v, want %+v", got, want)
	}
}

// How a revive restores a declared command, for each mode and both ways of being allowed to run it.
//
// The rows that matter most are the refusals: on_restore = "command" without the allowlist or a per-session
// request prefills rather than runs, and "shell" ignores the declaration entirely, so a program cannot
// promote itself into executing at the next boot.
func TestInheritForRestoreDeclaredCommand(t *testing.T) {
	claude := []string{"claude", "--resume", "abc"}
	tests := []struct {
		name       string
		configured RestoreAction
		perSession RestoreAction
		safe       bool
		want       *typedCommand
	}{
		{name: "shell ignores it", configured: RestoreShell},
		{name: "none ignores it", configured: RestoreNone},
		{name: "prefill", configured: RestorePrefill, want: &typedCommand{argv: claude, declared: true}},
		{name: "prefill even when allowlisted", configured: RestorePrefill, safe: true,
			want: &typedCommand{argv: claude, declared: true}},
		{name: "command without permission prefills", configured: RestoreCommand,
			want: &typedCommand{argv: claude, declared: true}},
		{name: "command allowlisted runs", configured: RestoreCommand, safe: true,
			want: &typedCommand{argv: claude, run: true, declared: true}},
		{name: "command asked for on the session runs", configured: RestoreShell, perSession: RestoreCommand,
			want: &typedCommand{argv: claude, run: true, declared: true, granted: true}},
		{name: "prefill asked for on the session", configured: RestoreShell, perSession: RestorePrefill,
			want: &typedCommand{argv: claude, declared: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, dirs := newTestManager(t, nil)
			policy := testPolicy()
			policy.OnRestore = tt.configured
			policy.CommandIsSafeToRerun = func([]string) bool { return tt.safe }
			mgr.SetPersistPolicy(policy)

			rec := store.Session{
				ID:      "revived",
				LogPath: dirs.SessionLog("revived"),
				Resume:  store.ResumeCommand{Argv: claude, Frame: "old-1"},
			}
			got := mgr.inheritForRestore(OpenOptions{Ref: "revived", OnRestore: tt.perSession}, rec)
			if !reflect.DeepEqual(got.typeOnStart, tt.want) {
				t.Errorf("typeOnStart = %+v, want %+v", got.typeOnStart, tt.want)
			}
			// Typed into a shell, never exec'd as the session's command, so quitting the resumed program
			// leaves a prompt. "none" is the one row that runs something, its holding process.
			if tt.configured != RestoreNone && len(got.Command) != 0 {
				t.Errorf("Command = %v, want none: a declared command is typed, not exec'd", got.Command)
			}
		})
	}
}

// A shell's first prompt marker is enough for a revive to know its line editor is up.
//
// The marker changes no published state, so it never reaches noteCommand, and recording it there meant a
// revive always waited out typePromptWait and typed blind: 13.8s against 3.8s in the e2e test, which
// passed either way.
func TestALonePromptMarkerIsSeen(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	if sess.PromptSeen() {
		t.Fatal("PromptSeen() = true before any output")
	}
	sess.processChunk([]byte("\x1b]133;A\x07$ "), 0)
	if !sess.PromptSeen() {
		t.Error("PromptSeen() = false after a prompt marker")
	}
}

// The typed command is the line the session's own shell started, and ends with its frame.
//
// Whole values at each step: the line, the frame it is bound to, and nothing after the program returns,
// which is what keeps a program the user quit from coming back on the next revive.
func TestATypedCommandLastsAsLongAsItsFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("sh-1", "sleep 999", false), 0)
	if got, want := sess.Typed(), (store.TypedCommand{Line: "sleep 999", Frame: "sh-1"}); got != want {
		t.Fatalf("Typed() = %+v, want %+v", got, want)
	}
	sess.processChunk(frame("sh-1", "", true), 0)
	if got := sess.Typed(); got != (store.TypedCommand{}) {
		t.Errorf("Typed() after the command returned = %+v, want the zero value", got)
	}
}

// Only the outermost frame is the session's own. One above it is written by a shell further in, the far side
// of an ssh or a nested `cm attach`, and typed back into this session's new shell it would run that program
// here; the outermost one is the ssh or the attach itself, which typed back reconnects.
func TestATypedCommandIsTheOutermostFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	want := store.TypedCommand{Line: "cm attach child", Frame: "p-1"}

	sess.processChunk(frame("p-1", "cm attach child", false), 0)
	sess.beginHosting("child")
	sess.processChunk(frame("c-1", "vim notes.md", false), 0)
	if got := sess.Typed(); got != want {
		t.Errorf("Typed() with a nested session's command open = %+v, want the parent's own, %+v", got, want)
	}
	sess.processChunk(frame("c-1", "", true), 0)
	if got := sess.Typed(); got != want {
		t.Errorf("Typed() after the nested command returned = %+v, want %+v", got, want)
	}
	sess.endHosting("child")
	sess.processChunk(frame("p-1", "", true), 0)
	if got := sess.Typed(); got != (store.TypedCommand{}) {
		t.Errorf("Typed() after the attach returned = %+v, want the zero value", got)
	}
}

// Only a session whose own program is a shell records a typed command. One spawned as a program, like
// `kitten ssh host`, has no shell of its own, so its outermost frame is written by the remote shell, and a
// remote `vim` recorded here would come back as a local one; it records nothing, and its spawn argv is what
// comes back.
//
// The frames are written straight into the session, as a remote shell's would arrive through the pty. The
// whole list is compared, so the shell rows guard against a rule that refused everything.
func TestOnlyAShellSessionRecordsATypedCommand(t *testing.T) {
	vim := store.TypedCommand{Line: "vim notes.md", Frame: "r-1"}
	var got, want []store.TypedCommand
	for _, tc := range []struct {
		command []string
		want    store.TypedCommand
	}{
		{nil, vim},
		{[]string{"/bin/zsh", "-l"}, vim},
		{[]string{"kitten", "ssh", "host"}, store.TypedCommand{}},
		{[]string{"/bin/sh", "-c", "exec ssh host"}, store.TypedCommand{}},
		// kitty's shell launcher, as a launch wrapper spawns a shell window.
		{[]string{"/Applications/kitty.app/Contents/MacOS/kitten", "run-shell", "--shell", "/opt/homebrew/bin/zsh",
			"--shell-integration", "disabled", "--cwd=/tmp"}, vim},
		{[]string{"kitten", "run-shell", "--shell=zsh", "nvim"}, store.TypedCommand{}},
	} {
		sess := newNestedTestSession(t, &fakeTerminal{})
		sess.record.Command = tc.command
		sess.processChunk(frame("r-1", "vim notes.md", false), 0)
		got = append(got, sess.Typed())
		want = append(want, tc.want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Typed() by spawn argv = %+v\nwant %+v", got, want)
	}
}

// kitty's shell launcher is a shell when nothing but its own options follow it, and a program once a command
// does, as is anything that only looks like it.
func TestIsJustAShellReadsKittysShellLauncher(t *testing.T) {
	kitten := "/Applications/kitty.app/Contents/MacOS/kitten"
	var got []bool
	for _, argv := range [][]string{
		{kitten, "run-shell"},
		{kitten, "run-shell", "--shell", "/opt/homebrew/bin/zsh", "--shell-integration", "disabled", "--cwd=/tmp"},
		{"kitten", "run-shell", "--env", "A=b", "--env=C", "--inject-self-onto-path=never", "--"},
		{kitten, "run-shell", "--shell=zsh", "nvim", "notes.md"},
		{kitten, "run-shell", "--", "nvim"},
		{kitten, "run-shell", "--shell"},
		{kitten, "run-shell", "--hold"},
		{kitten, "ssh", "host"},
		{"run-shell", "kitten"},
	} {
		got = append(got, isJustAShell(argv))
	}
	if want := []bool{true, true, true, false, false, false, false, false, false}; !reflect.DeepEqual(got, want) {
		t.Errorf("isJustAShell = %v, want %v", got, want)
	}
}

// A kill that keeps the session freezes the typed command, as it freezes the declaration: the hangup closes
// the frame, and that close must not erase what the revive brings back.
func TestAKillThatKeepsTheSessionKeepsItsTypedCommand(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	want := store.TypedCommand{Line: "sleep 999", Frame: "sh-1"}

	sess.processChunk(frame("sh-1", "sleep 999", false), 0)
	sess.keepRecord.Store(true)
	sess.processChunk(frame("sh-1", "", true), 0)
	if got := sess.Typed(); got != want {
		t.Errorf("Typed() after the frame closed under a keeping kill = %+v, want %+v", got, want)
	}
}

// `restore-with --clear` leaves an empty declaration bound to the frame it was run in, so a revive brings
// nothing back for that frame, not even the typed command; and it ends with that frame like a declaration.
func TestAClearedDeclarationIsBoundToItsFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("sh-1", "terraform apply", false), 0)
	sess.SetResume([]string{"terraform", "plan"})
	sess.ClearResume()
	if got, want := sess.Resume(), (store.ResumeCommand{Frame: "sh-1"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Resume() after a clear = %+v, want %+v", got, want)
	}
	sess.processChunk(frame("sh-1", "", true), 0)
	if got := sess.Resume(); !reflect.DeepEqual(got, store.ResumeCommand{}) {
		t.Errorf("Resume() after the cleared frame returned = %+v, want the zero value", got)
	}
}

// What a revive brings back when nothing was asked for, in order: the declared resume command, else the typed
// command, else a spawn argv that is a program, else a shell; and the policy each gets.
//
// The whole of what the revive starts is compared each time, the typed line and the exec'd command together,
// since the two failures here are bringing back the wrong one and bringing back two. The rows that matter most
// are the precedence rows: a declaration or a clear beats a typed command, and a typed command beats the
// spawn argv.
func TestInheritForRestorePrecedence(t *testing.T) {
	claude := store.ResumeCommand{Argv: []string{"claude", "--resume", "abc"}, Frame: "sh-1"}
	cleared := store.ResumeCommand{Frame: "sh-1"}
	typed := store.TypedCommand{Line: "sleep 999", Frame: "sh-1"}
	htop := []string{"htop", "-d", "5"}
	long := strings.Repeat("x", osc.MaxFrameArgv)

	type restored struct {
		Command     []string
		TypeOnStart *typedCommand
	}
	tests := []struct {
		name    string
		mode    RestoreAction
		safe    []string
		resume  store.ResumeCommand
		typed   store.TypedCommand
		command []string
		want    restored
	}{
		{name: "declared beats typed", mode: RestoreCommand, safe: []string{"sleep"}, resume: claude, typed: typed,
			want: restored{TypeOnStart: &typedCommand{argv: claude.Argv, declared: true}}},
		{name: "a clear beats typed", mode: RestoreCommand, safe: []string{"sleep"}, resume: cleared, typed: typed,
			command: htop},
		{name: "typed allowlisted runs", mode: RestoreCommand, safe: []string{"sleep"}, typed: typed,
			command: []string{"/bin/zsh", "-l"},
			want:    restored{TypeOnStart: &typedCommand{line: "sleep 999", run: true}}},
		{name: "a session that is a program comes back as it, not as a typed line", mode: RestoreCommand,
			safe: []string{"sleep", "htop"}, typed: typed, command: htop, want: restored{Command: htop}},
		{name: "typed not allowlisted is prefilled", mode: RestoreCommand, typed: typed,
			want: restored{TypeOnStart: &typedCommand{line: "sleep 999"}}},
		{name: "typed under prefill is prefilled", mode: RestorePrefill, safe: []string{"sleep"}, typed: typed,
			want: restored{TypeOnStart: &typedCommand{line: "sleep 999"}}},
		{name: "typed under shell is ignored", mode: RestoreShell, safe: []string{"sleep"}, typed: typed},
		{name: "a typed line that may be cut off is only prefilled", mode: RestoreCommand, safe: []string{"x"},
			typed: store.TypedCommand{Line: long, Frame: "sh-1"},
			want:  restored{TypeOnStart: &typedCommand{line: long}}},
		{name: "spawn argv allowlisted runs in place", mode: RestoreCommand, safe: []string{"htop"}, command: htop,
			want: restored{Command: htop}},
		{name: "spawn argv not allowlisted is prefilled", mode: RestoreCommand, command: htop,
			want: restored{TypeOnStart: &typedCommand{argv: htop}}},
		{name: "a spawn argv that is just a shell brings nothing back", mode: RestoreCommand,
			safe: []string{"zsh"}, command: []string{"/bin/zsh", "-l"}},
		{name: "kitty's shell launcher brings nothing back", mode: RestoreCommand, safe: []string{"kitten"},
			command: []string{"kitten", "run-shell", "--shell", "zsh", "--cwd=/tmp"}},
		{name: "kitty's shell launcher brings back its typed command", mode: RestoreCommand,
			safe: []string{"sleep"}, typed: typed, command: []string{"kitten", "run-shell", "--shell", "zsh"},
			want: restored{TypeOnStart: &typedCommand{line: "sleep 999", run: true}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, dirs := newTestManager(t, nil)
			policy := testPolicy()
			policy.OnRestore = tt.mode
			policy.CommandIsSafeToRerun = func(argv []string) bool {
				return len(argv) > 0 && slices.Contains(tt.safe, filepath.Base(argv[0]))
			}
			mgr.SetPersistPolicy(policy)

			rec := store.Session{
				ID: "revived", LogPath: dirs.SessionLog("revived"),
				Resume: tt.resume, Typed: tt.typed, Command: tt.command,
			}
			got := mgr.inheritForRestore(OpenOptions{Ref: "revived"}, rec)
			if r := (restored{Command: got.Command, TypeOnStart: got.typeOnStart}); !reflect.DeepEqual(r, tt.want) {
				t.Errorf("restored %+v (typeOnStart %+v), want %+v (typeOnStart %+v)",
					r, r.TypeOnStart, tt.want, tt.want.TypeOnStart)
			}
		})
	}
}

// A session's own --on-restore, kept with its record, decides a revive that names none, and counts as asked
// for explicitly; a revive that names one wins, and becomes the session's.
//
// The regression: only the reviving request's value was seen, and a terminal restoring a window passes none,
// so a session created with --on-restore=command came back as a shell after a reboot unless its program was in
// safe_commands. Nothing is allowlisted here, so only the kept value can let the spawn argv run.
func TestInheritForRestoreKeepsTheSessionsOnRestore(t *testing.T) {
	ssh := []string{"sh", "-c", `echo "CONNECTED [$1]"; exec sleep 999`, "sh", "host a"}

	type restored struct {
		OnRestore   RestoreAction
		Command     []string
		TypeOnStart *typedCommand
	}
	tests := []struct {
		name       string
		configured RestoreAction
		kept       string
		reviving   RestoreAction
		want       restored
	}{
		{name: "the kept command runs without the allowlist", configured: RestoreCommand, kept: "command",
			want: restored{OnRestore: RestoreCommand, Command: ssh}},
		{name: "the kept command applies over a shell default", configured: RestoreShell, kept: "command",
			want: restored{OnRestore: RestoreCommand, Command: ssh}},
		{name: "a reviving shell overrides the kept command", configured: RestoreCommand, kept: "command",
			reviving: RestoreShell, want: restored{OnRestore: RestoreShell}},
		{name: "with nothing kept the configured command prefills", configured: RestoreCommand,
			want: restored{TypeOnStart: &typedCommand{argv: ssh}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, dirs := newTestManager(t, nil)
			policy := testPolicy()
			policy.OnRestore = tt.configured
			mgr.SetPersistPolicy(policy)

			rec := store.Session{
				ID: "ssh1", LogPath: dirs.SessionLog("ssh1"), Command: ssh, OnRestore: tt.kept,
			}
			got := mgr.inheritForRestore(OpenOptions{Ref: "ssh1", OnRestore: tt.reviving}, rec)
			r := restored{OnRestore: got.OnRestore, Command: got.Command, TypeOnStart: got.typeOnStart}
			if !reflect.DeepEqual(r, tt.want) {
				t.Errorf("restored %+v (typeOnStart %+v), want %+v (typeOnStart %+v)",
					r, r.TypeOnStart, tt.want, tt.want.TypeOnStart)
			}
		})
	}
}

// A session's kept --on-restore=command lets run without the allowlist only what the attach that set it
// started. From an --in-shell attach that is the command it typed and a declaration that command made, bound
// to their frame; a command typed after it returned is the user's and gets the configured policy, here
// prefill since nothing is allowlisted. From a plain attach it is the spawn argv, and a declaration the
// session's program made outside any frame. The reviving request's own --on-restore=command still covers
// everything that revive brings back.
//
// The regression: the kept value counted as asking for everything, so whatever was typed into an --in-shell
// session's shell later ran on every revive, allowlisted or not.
func TestTheKeptOnRestoreCoversOnlyWhatItsAttachStarted(t *testing.T) {
	claude := []string{"claude", "--resume", "abc"}
	ssh := []string{"kitten", "ssh", "host"}
	tests := []struct {
		name   string
		rec    store.Session
		asked  RestoreAction
		want   *typedCommand
		wantEx []string
	}{
		{name: "an --in-shell attach's command runs",
			rec:  store.Session{Typed: store.TypedCommand{Line: "sleep 999", Frame: "sh-1"}, OnRestoreFrame: "sh-1"},
			want: &typedCommand{line: "sleep 999", run: true, granted: true}},
		{name: "a command typed after it returned is prefilled",
			rec:  store.Session{Typed: store.TypedCommand{Line: "sleep 998", Frame: "sh-2"}},
			want: &typedCommand{line: "sleep 998"}},
		{name: "a grant bound to another frame does not cover it",
			rec:  store.Session{Typed: store.TypedCommand{Line: "sleep 998", Frame: "sh-2"}, OnRestoreFrame: "sh-1"},
			want: &typedCommand{line: "sleep 998"}},
		{name: "a declaration in the granted frame runs",
			rec:  store.Session{Resume: store.ResumeCommand{Argv: claude, Frame: "sh-1"}, OnRestoreFrame: "sh-1"},
			want: &typedCommand{argv: claude, run: true, declared: true, granted: true}},
		{name: "a declaration from a command typed later is prefilled",
			rec:  store.Session{Resume: store.ResumeCommand{Argv: claude, Frame: "sh-2"}},
			want: &typedCommand{argv: claude, declared: true}},
		{name: "a program's own declaration runs",
			rec:  store.Session{Command: ssh, Resume: store.ResumeCommand{Argv: claude}},
			want: &typedCommand{argv: claude, run: true, declared: true, granted: true}},
		{name: "a shell's declaration outside any frame is prefilled",
			rec:  store.Session{Resume: store.ResumeCommand{Argv: claude}},
			want: &typedCommand{argv: claude, declared: true}},
		{name: "a plain attach's spawn argv runs",
			rec: store.Session{Command: ssh}, wantEx: ssh},
		{name: "an --in-shell grant does not cover the spawn argv",
			rec:  store.Session{Command: ssh, OnRestoreFrame: "sh-1"},
			want: &typedCommand{argv: ssh}},
		{name: "the reviving request's own command covers a later command",
			rec:   store.Session{Typed: store.TypedCommand{Line: "sleep 998", Frame: "sh-2"}},
			asked: RestoreCommand, want: &typedCommand{line: "sleep 998", run: true, granted: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _, dirs := newTestManager(t, nil)
			policy := testPolicy()
			policy.OnRestore = RestoreCommand
			mgr.SetPersistPolicy(policy)

			rec := tt.rec
			rec.ID, rec.LogPath, rec.OnRestore = "kept", dirs.SessionLog("kept"), "command"
			got := mgr.inheritForRestore(OpenOptions{Ref: "kept", OnRestore: tt.asked}, rec)
			if !reflect.DeepEqual(got.typeOnStart, tt.want) || !reflect.DeepEqual(got.Command, tt.wantEx) {
				t.Errorf("restored command %q, typeOnStart %+v; want %q, %+v",
					got.Command, got.typeOnStart, tt.wantEx, tt.want)
			}
		})
	}
}

// The grant an --in-shell attach or a revive leaves waiting binds to the next frame, the typed command's,
// and ends when that frame closes, back to the spawn argv; a kill that keeps the session freezes it, as it
// freezes the declaration, since the hangup closes the frame.
func TestAnOnRestoreGrantLastsAsLongAsItsFrame(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sess.onRestoreFrame = resumeNextFrame
	var got []string
	sess.processChunk(frame("sh-1", "sleep 999", false), 0)
	got = append(got, sess.OnRestoreFrame())
	sess.processChunk(frame("sh-1", "", true), 0)
	got = append(got, sess.OnRestoreFrame())

	kept := newNestedTestSession(t, &fakeTerminal{})
	kept.onRestoreFrame = resumeNextFrame
	kept.processChunk(frame("sh-1", "sleep 999", false), 0)
	kept.keepRecord.Store(true)
	kept.processChunk(frame("sh-1", "", true), 0)
	got = append(got, kept.OnRestoreFrame())

	if want := []string{"sh-1", "", "sh-1"}; !slices.Equal(got, want) {
		t.Errorf("on_restore_frame bound, after its frame closed, and frozen = %q, want %q", got, want)
	}
}

// A typed line runs on a revive only when it is a plain program invocation; anything a shell would read as
// more is prefilled, under on_restore = "command" with the program allowlisted.
//
// Each row is the whole outcome, the line typed back and whether it is submitted. The run rows matter as
// much as the refusals: a rule that refused everything would pass every prefill row.
func TestATypedLineRunsOnlyWhenPlain(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	policy := testPolicy()
	policy.OnRestore = RestoreCommand
	policy.CommandIsSafeToRerun = func(argv []string) bool {
		return len(argv) > 0 && slices.Contains([]string{"sleep", "gr", "nvim", "ls", "echo", "env"},
			filepath.Base(argv[0]))
	}
	mgr.SetPersistPolicy(policy)

	for _, tc := range []struct {
		line string
		run  bool
	}{
		// Plain: a program word and argument words, quoted or not, with ~/ leading a word.
		{"sleep 999", true},
		{"gr --watch", true},
		{"nvim 'my notes.md' \"other file\"", true},
		{"'gr' --watch", true},
		{"ls ~/src ~", true},
		{"env A=b nvim", true},
		{"echo a=b c,d e.f@g:h%i+j", true},
		{"  sleep\t999  ", true},
		// Not on the allowlist, which is the old rule and still applies.
		{"htop", false},
		// Operators and grouping.
		{"sleep 1; echo x", false},
		{"sleep 1 | cat", false},
		{"sleep 1 & echo x", false},
		{"sleep 1 && echo x", false},
		{"sleep 1 || echo x", false},
		{"echo x > f", false},
		{"echo x >> f", false},
		{"nvim < f", false},
		{"cat <<EOF", false},
		{"(sleep 1)", false},
		{"{ sleep 1; }", false},
		// Expansions, including inside double quotes.
		{"echo $HOME", false},
		{"echo \"$HOME\"", false},
		{"echo $(id)", false},
		{"echo `id`", false},
		// Globs and zsh's extended ones.
		{"ls *.go", false},
		{"ls a?", false},
		{"ls [ab]", false},
		{"ls a#b", false},
		{"ls a~b", false},
		// History.
		{"echo !!", false},
		{"echo \"hi!\"", false},
		{"^foo^bar", false},
		// Continuations and newlines.
		{"sleep \\\n1", false},
		{"sleep 1\necho x", false},
		{"sleep 1 \\", false},
		// Assignments, ~user and equals expansion.
		{"FOO=bar sleep 1", false},
		{"ls ~root", false},
		{"ls =sleep", false},
		// Unbalanced quotes, and nothing at all.
		{"echo 'unterminated", false},
		{"echo \"unterminated", false},
		{"", false},
		// Quoted, the same characters are plain words.
		{"echo 'a; b | c $d *'", true},
	} {
		got := mgr.restoreTyped(tc.line, RestoreCommand, false)
		if want := (&typedCommand{line: tc.line, run: tc.run}); !reflect.DeepEqual(got, want) {
			t.Errorf("restoreTyped(%q) = %+v, want %+v", tc.line, got, want)
		}
	}

	// And a session that asked for --on-restore=command does not get past it either.
	line := "sleep 1; echo x"
	if got, want := mgr.restoreTyped(line, RestoreCommand, true), (&typedCommand{line: line, granted: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("restoreTyped(%q) with the session asking for command = %+v, want %+v", line, got, want)
	}
}

// A command frame open in a new shell before its restore command is typed means something is in the
// foreground, so nothing is typed, and the declaration and grant carried for the typed command, which bound
// to that frame instead, are withdrawn. Without a frame the command is typed as before.
func TestARestoreCommandIsNotTypedIntoAProgramThatStartedFirst(t *testing.T) {
	claude := []string{"claude", "--resume", "abc"}
	carried := typedCommand{argv: claude, run: true, declared: true, granted: true}

	idle := newNestedTestSession(t, &fakeTerminal{})
	idle.resume = store.ResumeCommand{Argv: claude, Frame: resumeNextFrame}
	if running, ok := idle.startedOnItsOwn(carried); ok {
		t.Errorf("startedOnItsOwn() with no frame open = %q, true; want false", running)
	}

	busy := newNestedTestSession(t, &fakeTerminal{})
	busy.resume = store.ResumeCommand{Argv: claude, Frame: resumeNextFrame}
	busy.onRestoreFrame = resumeNextFrame
	busy.processChunk(frame("sh-1", "nvim notes.md", false), 0)
	running, ok := busy.startedOnItsOwn(carried)
	got := []any{running, ok, busy.Resume(), busy.OnRestoreFrame()}
	want := []any{"nvim notes.md", true, store.ResumeCommand{}, ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("startedOnItsOwn() with nvim running: running, ok, resume, grant = %+v\nwant %+v", got, want)
	}
}
