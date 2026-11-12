package server

import (
	"reflect"
	"testing"

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
		{name: "prefill", configured: RestorePrefill, want: &typedCommand{argv: claude}},
		{name: "prefill even when allowlisted", configured: RestorePrefill, safe: true,
			want: &typedCommand{argv: claude}},
		{name: "command without permission prefills", configured: RestoreCommand,
			want: &typedCommand{argv: claude}},
		{name: "command allowlisted runs", configured: RestoreCommand, safe: true,
			want: &typedCommand{argv: claude, run: true}},
		{name: "command asked for on the session runs", configured: RestoreShell, perSession: RestoreCommand,
			want: &typedCommand{argv: claude, run: true}},
		{name: "prefill asked for on the session", configured: RestoreShell, perSession: RestorePrefill,
			want: &typedCommand{argv: claude}},
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
