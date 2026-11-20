package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// typedEnv is a persisting server whose sessions start zsh with cm's shell integration loaded and a prompt
// that marks itself, so command frames and prompt marks are what a real interactive shell sends, with
// safe_commands set to safe.
//
// zsh rather than /bin/sh, because the frames under test come from the integration's hooks and dash has none.
// The shell is the server's default through SHELL, so a revive, which starts the default shell, gets the same
// one; HOME points at a .zshrc that loads the integration, for every client, so a restored shell has it too.
func typedEnv(t *testing.T, safe ...string) *env {
	t.Helper()
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	e := newEnv(t)
	quoted := make([]string, 0, len(safe))
	for _, s := range safe {
		quoted = append(quoted, `"`+s+`"`)
	}
	e.writeConfig("[persist]\nenabled = true\non_restore = \"command\"\nsafe_commands = [" +
		strings.Join(quoted, ", ") + "]\n")

	home := filepath.Join(e.state, "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	// The prompt carries OSC 133;A, which cm's integration leaves to the prompt, as kitty's or starship's
	// does. Without it a revive waits 10s before typing, which passes and hides that the shell never said
	// it was at its line editor.
	rc := "eval \"$(" + e.bin + " shell-init zsh)\"\nPROMPT=$'%{\\e]133;A\\a%}%% '\n"
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte(rc), 0o600); err != nil {
		t.Fatal(err)
	}
	e.extraEnv = append(e.extraEnv, "HOME="+home, "SHELL="+zsh, "ZDOTDIR="+home)
	// Restarted so the server picks up the config and the environment above, both read when it starts.
	e.restartServer()
	return e
}

// startTyped starts a persisted session, waits for its integration to load, and types line into it.
func startTyped(e *env, name, line string) {
	e.t.Helper()
	e.mustRun("attach", "--no-attach", "--persist", name)
	// Printed in pieces, so what is waited for is the command's output rather than the echo of the line.
	e.mustRun("send", name, `printf 'LOAD%s\n' DONE`, "--enter")
	e.waitForHistory(name, "LOADDONE")
	e.mustRun("send", name, line, "--enter")
}

// typedCommand is `cm info NAME --field typed_command` without its newline.
func typedCommand(e *env, name string) string {
	e.t.Helper()
	return strings.TrimSuffix(e.mustRun("info", name, "--field", "typed_command"), "\n")
}

// A command typed into a shell comes back with the session: run when its program is in safe_commands, typed on
// the prompt unrun when it is not.
//
// The regression is that a revive could only bring back the spawn argv, which for a session started as a shell
// is the shell, so `sleep` in safe_commands never matched anything and a shelved agent window came back as
// a bare prompt. Shelved, then restored by an attach that names a command of its own, which is ignored.
func TestARevivedShellBringsBackItsTypedCommand(t *testing.T) {
	skipIfShort(t)
	for _, tc := range []struct {
		name string
		safe []string
		runs bool
	}{
		{"allowlisted runs", []string{"sleep"}, true},
		{"not allowlisted is prefilled", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := typedEnv(t, tc.safe...)
			startTyped(e, "work", "sleep 999")
			e.waitFor("the typed command to be recorded", 15*time.Second, func() bool {
				return typedCommand(e, "work") == "sleep 999"
			})

			e.mustRun("shelve", "work")
			if got := typedCommand(e, "work"); got != "sleep 999" {
				t.Fatalf("typed_command after shelving = %q, want it kept as %q", got, "sleep 999")
			}

			e.mustRun("attach", "--no-attach", "work", "--", "/bin/sh")
			if tc.runs {
				e.waitFor("the revived shell to be running the typed command", 15*time.Second, func() bool {
					s, ok := e.session("work")
					return ok && s.State == "running" && len(s.Location) == 1 && s.Location[0].Argv == "sleep 999"
				})
				return
			}
			// Typed on the prompt: the history's last line is the prompt with the command on it.
			e.waitFor("the command to be typed on the revived prompt", 15*time.Second, func() bool {
				h := strings.TrimRight(e.run("history", "work").stdout, " \n")
				return strings.HasSuffix(h, "% sleep 999")
			})
			time.Sleep(time.Second)
			s, _ := e.session("work")
			if len(s.Location) != 0 || typedCommand(e, "work") != "" {
				t.Errorf("a prefilled command ran: location %+v, typed_command %q", s.Location, typedCommand(e, "work"))
			}
		})
	}
}

// A command that finished before the session ended is not brought back: its frame closed at the prompt, which
// is the user saying they were done with it.
//
// echo is allowlisted, so a revive that did bring it back would run it, and its output would appear a second
// time after the replayed one.
func TestAFinishedTypedCommandIsNotBroughtBack(t *testing.T) {
	skipIfShort(t)
	e := typedEnv(t, "echo")
	startTyped(e, "work", "echo FINISHED-$((40+2))")
	e.waitForHistory("work", "FINISHED-42")
	if got := typedCommand(e, "work"); got != "" {
		t.Fatalf("typed_command at the prompt = %q, want empty", got)
	}
	e.mustRun("kill", "work", "--keep")
	e.waitFor("the session to have ended", 15*time.Second, func() bool {
		s, ok := e.session("work")
		return ok && s.State != "running"
	})

	e.mustRun("attach", "--no-attach", "work")
	e.mustRun("send", "work", `printf 'BACK%s\n' AGAIN`, "--enter")
	history := e.waitForHistory("work", "BACKAGAIN")
	if n := strings.Count(history, "\nFINISHED-42"); n != 1 {
		t.Errorf("FINISHED-42 printed %d times after the revive, want once, the replayed output; history:\n%s",
			n, history)
	}
}

// A typed line that is a shell expression comes back prefilled, not run, even with its program allowlisted:
// the shell would run all of it, and `sleep` in safe_commands says nothing about what follows the `;`.
//
// Shelved mid-sleep, so the frame is open and the line is kept. Run, the line would print x a second after
// the revive; prefilled, it sits on the prompt.
func TestATypedShellExpressionIsOnlyPrefilled(t *testing.T) {
	skipIfShort(t)
	e := typedEnv(t, "sleep")
	startTyped(e, "work", "sleep 999; echo x")
	e.waitFor("the typed command to be recorded", 15*time.Second, func() bool {
		return typedCommand(e, "work") == "sleep 999; echo x"
	})
	e.mustRun("shelve", "work")

	e.mustRun("attach", "--no-attach", "work")
	e.waitFor("the line to be typed on the revived prompt", 15*time.Second, func() bool {
		h := strings.TrimRight(e.run("history", "work").stdout, " \n")
		return strings.HasSuffix(h, "% sleep 999; echo x")
	})
	time.Sleep(time.Second)
	if s, _ := e.session("work"); len(s.Location) != 0 {
		t.Errorf("the prefilled expression ran: location %+v", s.Location)
	}
}

// A session spawned through kitty's shell launcher is a shell window: its typed command is recorded, and a
// revive with nothing running comes back as a plain shell.
//
// Launch wrappers spawn shell windows as `kitten run-shell --shell zsh ... --cwd=DIR`, and such a session
// was read as a program, so its typed commands were never kept and a revive prefilled the launcher's own argv
// at the new prompt. kitten is stood in for by a script named kitten that starts the shell --shell names,
// which is all run-shell does with integration disabled.
func TestAKittyShellLauncherSessionIsAShell(t *testing.T) {
	skipIfShort(t)
	e := typedEnv(t)
	zsh, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}
	bin := filepath.Join(e.state, "kitty")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	kitten := filepath.Join(bin, "kitten")
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do [ \"$1\" = --shell ] && shift && sh=$1; shift; done\nexec \"$sh\"\n"
	if err := os.WriteFile(kitten, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	e.mustRun("attach", "--no-attach", "--persist", "work", "--",
		kitten, "run-shell", "--shell", zsh, "--shell-integration", "disabled", "--cwd="+e.state)
	e.mustRun("send", "work", `printf 'LOAD%s\n' DONE`, "--enter")
	e.waitForHistory("work", "LOADDONE")
	e.mustRun("send", "work", "sleep 999", "--enter")
	e.waitFor("the typed command to be recorded", 15*time.Second, func() bool {
		return typedCommand(e, "work") == "sleep 999"
	})
	e.mustRun("send", "work", "--key", "ctrl-c")
	e.waitFor("the typed command to have returned", 15*time.Second, func() bool {
		return typedCommand(e, "work") == ""
	})
	e.mustRun("shelve", "work")

	e.mustRun("attach", "--no-attach", "work")
	e.mustRun("send", "work", `printf 'BACK%s\n' AGAIN`, "--enter")
	history := e.waitForHistory("work", "BACKAGAIN")
	if strings.Contains(history, "run-shell") {
		t.Errorf("the launcher's argv came back on the revived prompt; history:\n%s", history)
	}
	e.mustRun("kill", "work")
}
