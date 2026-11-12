package e2e

import (
	"strings"
	"testing"
	"time"
)

// `attach --in-shell` types the command into the session's shell, so the shell outlives it.
//
// The case it exists for is kitty's launch --hold, which ran `sh -c '"$@"; exec $SHELL -l' sh gr`: the
// program started before any shell, so no preexec hook ran, `cm list` showed nothing running, and `cm doctor`
// reported the session as having no shell integration. Without the flag this command is the session's
// whole life, so the session ending is what a regression looks like here.
func TestAttachInShellTypesTheCommandIntoTheShell(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	name := "inshell"

	e.mustRun(append(append([]string{"attach", "--no-attach", "--in-shell", name}, e.markedPromptEnv()...),
		"--", "printf", `%s-%s\n`, "INSHELL", "a b")...)

	// Typed, so the line is echoed at a prompt, then run. The output differs from the typed line so the echo
	// cannot satisfy the second wait.
	e.waitForHistory(name, `printf '%s-%s\n' INSHELL 'a b'`)
	e.waitForHistory(name, "INSHELL-a b")
	// The shell prompting again after the command is the part that distinguishes typing from exec.
	e.waitFor("the shell to prompt after the command", 15*time.Second, func() bool {
		h := e.run("history", name).stdout
		_, after, ok := strings.Cut(h, "INSHELL-a b")
		return ok && strings.Contains(after, promptMarker)
	})
	if s, ok := e.session(name); !ok || s.State != "running" {
		t.Errorf("session = %+v, %v after its command finished, want it running", s, ok)
	}
	e.mustRun("kill", name)
}

// --in-shell with nothing to type is refused rather than accepted and ignored.
func TestAttachInShellNeedsACommand(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	res := e.run("attach", "--no-attach", "--in-shell", "empty")
	if res.code == 0 || !strings.Contains(res.stderr, "--in-shell needs a command") {
		t.Errorf("attach --in-shell with no command = %+v, want it refused", res)
	}
}
