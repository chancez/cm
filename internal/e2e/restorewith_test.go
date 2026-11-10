package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// markedPromptEnv makes a session's default shell prompt with an OSC 133;A, so a revived shell tells cm it
// is at its line editor, which is what a revive waits for before typing a resume command.
//
// Through a .profile in a HOME of the test's own rather than PS1 in the environment, which shellPromptEnv
// uses for an explicit /bin/sh: the default shell starts as a login shell, and darwin's /etc/profile sources
// /etc/bashrc, which sets PS1 over whatever was inherited. A login sh reads ~/.profile after it on both
// platforms.
func (e *env) markedPromptEnv() []string {
	e.t.Helper()
	home := filepath.Join(e.state, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		e.t.Fatalf("MkdirAll() error = %v", err)
	}
	profile := "PS1=\"$(printf '\\033]133;A\\007')" + promptMarker + "> \"\n"
	if err := os.WriteFile(filepath.Join(home, ".profile"), []byte(profile), 0o600); err != nil {
		e.t.Fatalf("WriteFile() error = %v", err)
	}
	return []string{"--env", "HOME=" + home}
}

// A declared resume command comes back when the session is revived: typed into the new shell and run under
// --on-restore=command, typed and left on the prompt under prefill.
//
// Real processes rather than the unit seam, because the part that can break is the hand-off between them:
// the declaration has to reach the store from an RPC, survive the session ending, and be typed into a shell
// spawned by a different shim. The argument with a space is the one quoting would lose, and the output
// differs from the typed line so the echo of the line cannot satisfy the wait.
func TestARevivedSessionRestoresItsDeclaredCommand(t *testing.T) {
	skipIfShort(t)

	for _, tt := range []struct {
		mode string
		run  bool
	}{
		{"command", true},
		{"prefill", false},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			e := newEnv(t)
			name := "resumed-" + tt.mode

			e.mustRun(append([]string{"attach", "--no-attach", "--persist", name}, e.markedPromptEnv()...)...)
			e.waitForHistory(name, promptMarker)
			e.mustRun("restore-with", name, "--", "printf", `%s-%s\n`, "RESUMED", "a b")

			e.mustRun("kill", name, "--keep")
			e.waitFor("the session to have ended", 15*time.Second, func() bool {
				s, ok := e.session(name)
				return ok && s.State == "exited"
			})

			e.mustRun(append([]string{"attach", "--no-attach", "--on-restore", tt.mode, name},
				e.markedPromptEnv()...)...)

			// The typed line, which both modes produce. Quoted by shquote, so 'a b' arrives as one word.
			e.waitForHistory(name, `printf '%s-%s\n' RESUMED 'a b'`)
			if tt.run {
				e.waitForHistory(name, "RESUMED-a b")
			} else {
				// Long enough for a submitted line to have run: the wait above saw it typed.
				time.Sleep(time.Second)
				if h := e.run("history", name).stdout; strings.Contains(h, "RESUMED-a b") {
					t.Errorf("prefill ran the command; history:\n%s", h)
				}
			}
			e.mustRun("kill", name)
		})
	}
}
