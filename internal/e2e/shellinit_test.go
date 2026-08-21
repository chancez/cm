package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shellRC writes a startup file loading cm's integration and returns how to make the shell read it.
//
// Loaded the way the help documents it, since that is the part worth testing: `eval "$(cm shell-init ...)"`
// for the POSIX shells and `| source` for fish. Sourcing the file directly instead would pass while the
// documented form was broken, which is exactly what happened with the early-`return` guard.
//
// Returns the env flags and the shell argv separately because the two shells differ in how they are pointed
// at a startup file: zsh and fish take a directory in the environment, bash takes a --rcfile argument.
//
// Everything is written under the test's own state directory, so the developer's real configuration is
// neither needed nor consulted and the test states its own preconditions.
func (e *env) shellRC(t *testing.T, shell, path string) (env []string, argv []string) {
	t.Helper()

	dir := filepath.Join(e.state, shell+"-home")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	write := func(file, content string) {
		if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}

	switch shell {
	case "zsh":
		// compinit before the eval, which is the ordering `cm shell-init --help` documents. shell-init
		// prints the completions ahead of the integration, and the completions register a completion
		// function, so loading them first puts `command not found: compdef` on the session's very first
		// screen. Harmless to the integration and visible to anyone reading the session.
		write(filepath.Join(dir, ".zshrc"),
			"autoload -Uz compinit; compinit -D 2>/dev/null\n"+
				"eval \"$("+e.bin+" shell-init zsh)\"\n")
		return []string{"--env", "ZDOTDIR=" + dir}, []string{path}

	case "bash":
		rc := filepath.Join(dir, "bashrc")
		write(rc, "eval \"$("+e.bin+" shell-init bash)\"\n")
		// --rcfile rather than BASH_ENV, which was the first attempt and does not work: an *interactive*
		// bash ignores BASH_ENV and reads its rcfile instead, so the function was never defined and the
		// test failed as though the script were broken. Verified both ways before changing it, and
		// interactive is the case that matters since a session runs its shell on a pty.
		return nil, []string{path, "--rcfile", rc, "-i"}

	case "fish":
		conf := filepath.Join(dir, "fish")
		if err := os.MkdirAll(conf, 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		write(filepath.Join(conf, "config.fish"), e.bin+" shell-init fish | source\n")
		return []string{"--env", "XDG_CONFIG_HOME=" + dir}, []string{path}
	}

	t.Fatalf("no startup file known for %s", shell)
	return nil, nil
}

// shellPath finds a shell, skipping the test when it is absent.
//
// Resolved through `mise which` first, because a shell installed by mise is on PATH only as a shim, and a
// shim is not a shell: it re-execs mise, which resolves a version from the *current directory's* config. A
// session's shell runs with the session's working directory, so the shim finds no config and the shell exits
// with "No version is set for shim: fish" before it can load anything.
//
// That failure is worth spelling out because of how it presents: the session ends immediately, `cm read`
// then says the output is gone, and the test reports the integration as broken when the shell never ran.
// Diagnosing it from the symptom alone cost a detour, hence resolving the real binary here.
func shellPath(t *testing.T, shell string) string {
	t.Helper()

	if out, err := exec.Command("mise", "which", shell).Output(); err == nil {
		if path := strings.TrimSpace(string(out)); path != "" {
			return path
		}
	}

	path, err := exec.LookPath(shell)
	if err != nil {
		t.Skipf("%s is not installed", shell)
	}
	return path
}

// A report from the shell integration must reach cm and be visible in the session's state.
//
// The end-to-end property the integration exists for, exercised through a real shell in a real session
// rather than by feeding bytes to the parser. Both halves have been broken independently: the parser was
// fine while the shell script mangled its own output, so only the whole path is worth asserting on.
//
// Every shell is covered because the scripts are separate implementations of one contract, and each has had
// a distinct bug -- printf format interpolation, `return` outside a function, `exit` ending the shell.
func TestShellIntegrationReportsReachCM(t *testing.T) {
	skipIfShort(t)

	for _, shell := range []string{"zsh", "bash", "fish"} {
		t.Run(shell, func(t *testing.T) {
			path := shellPath(t, shell)
			e := newEnv(t)

			name := "report-" + shell
			envFlags, argv := e.shellRC(t, shell, path)
			args := append([]string{"attach", "--no-attach", name}, envFlags...)
			args = append(args, "--")
			args = append(args, argv...)
			e.mustRun(args...)

			// A detail carrying the characters with meaning on the wire: the field separator, the escape,
			// and printf's format specifier. Passed through a variable so the shell's own quoting, not this
			// test's, decides what the function receives.
			const detail = `a;b\c 50% x`
			var send string
			switch shell {
			case "fish":
				send = `set d 'a;b\c 50% x'; cm_report blocked $d`
			default:
				send = `d='a;b\c 50% x'; cm_report blocked "$d"`
			}
			e.mustRun("send", name, send, "--enter")

			// Polled rather than slept: the report travels shell -> pty -> shim -> server, and a fixed sleep
			// would be either flaky or slow.
			var got string
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				got = strings.TrimSpace(e.mustRun("info", name, "--field", "reported_state"))
				if got == "blocked" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if got != "blocked" {
				// The session listing and its output both, since the two failures look identical from the
				// state alone: a report that never arrived, and a shell that never started.
				r := e.run("read", name)
				t.Fatalf("reported_state = %q, want \"blocked\"\nsession output: %s%s\nlist: %s",
					got, r.stdout, r.stderr, e.run("list").stdout)
			}

			if want, got := detail, strings.TrimSpace(e.mustRun("info", name, "--field", "reported_detail")); got != want {
				t.Errorf("reported_detail = %q, want %q", got, want)
			}
			if want, got := shell, strings.TrimSpace(e.mustRun("info", name, "--field", "reported_source")); got != want {
				t.Errorf("reported_source = %q, want %q", got, want)
			}

			// Clearing must fall back to what cm derives, rather than leaving the session stuck blocked.
			e.mustRun("send", name, "cm_report clear", "--enter")

			deadline = time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				got = strings.TrimSpace(e.mustRun("info", name, "--field", "reported_state"))
				if got == "" {
					break
				}
				time.Sleep(100 * time.Millisecond)
			}
			if got != "" {
				t.Errorf("reported_state = %q after clear, want empty so cm falls back to what it derives", got)
			}
		})
	}
}

// `cm wait --until blocked` must return when the integration reports it.
//
// The reason blocked is worth reporting at all: it is the state cm cannot derive, so something waiting on
// it is the whole point. Asserted separately from the state being visible because a state that is readable
// but does not wake a waiter would still be useless.
func TestShellIntegrationUnblocksAWaiter(t *testing.T) {
	skipIfShort(t)

	path := shellPath(t, "zsh")
	e := newEnv(t)

	name := "wait-blocked"
	envFlags, argv := e.shellRC(t, "zsh", path)
	args := append([]string{"attach", "--no-attach", name}, envFlags...)
	args = append(args, "--")
	args = append(args, argv...)
	e.mustRun(args...)

	// The waiter starts first, so this tests being woken rather than observing an already-set state.
	done := make(chan result, 1)
	go func() {
		done <- e.run("wait", name, "--until", "blocked", "--timeout", "20s")
	}()

	// Given to the waiter before reporting, so a report arriving first would not be what satisfied it.
	time.Sleep(500 * time.Millisecond)
	e.mustRun("send", name, `cm_report blocked "needs input"`, "--enter")

	select {
	case r := <-done:
		if r.code != 0 {
			t.Errorf("wait exited %d, want 0\nstdout: %s\nstderr: %s", r.code, r.stdout, r.stderr)
		}
	case <-time.After(25 * time.Second):
		t.Fatalf("wait did not return after the session reported blocked\nsession output:\n%s",
			e.mustRun("read", name))
	}
}

// Loading the integration must not break a shell that is not in a cm session.
//
// The failure this guards against is the one most likely to reach a user's real dotfiles: the guard at the
// top of each script runs on every shell they open, cm session or not. An early `return` there silently
// skipped the rest of a zsh rc file and printed an error on every bash startup.
//
// Two forms are checked, because `cm shell-init` prints the completions ahead of the integration and only
// the integration claims to load anywhere:
//
//   - `--no-completions` is the integration alone, and must be silent in every shell with no setup at all.
//     That is the claim the help makes and the invariant this test was written for.
//   - The bundled default, loaded the way the help documents it, which in zsh means after compinit. The
//     completions register a completion function, so they need that machinery in place.
//
// Asserting only the bundled form and only the documented ordering would stop measuring the integration's
// own safety, which is the more important half: a user can load the completions wherever they like, but the
// integration is what runs in every shell.
//
// Run as a plain subprocess rather than in a session, since being outside one is the condition under test.
func TestShellIntegrationIsSafeOutsideASession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for _, shell := range []string{"zsh", "bash", "fish"} {
		t.Run(shell, func(t *testing.T) {
			for _, form := range []struct {
				name string
				args []string
				// setup runs before the eval, for the machinery a form needs.
				setup string
			}{
				{name: "integration alone", args: []string{"shell-init", shell, "--no-completions"}},
				{
					name: "bundled with completions",
					args: []string{"shell-init", shell},
					setup: map[string]string{
						// The ordering `cm shell-init --help` documents. Without it the completions half
						// prints "command not found: compdef", which is what a session's first screen
						// showed for real.
						"zsh": "autoload -Uz compinit; compinit -D 2>/dev/null\n",
					}[shell],
				},
			} {
				t.Run(form.name, func(t *testing.T) {
					path := shellPath(t, shell)

					script, err := exec.Command(e.bin, form.args...).Output()
					if err != nil {
						t.Fatalf("%s failed: %v", strings.Join(form.args, " "), err)
					}

					var load string
					if shell == "fish" {
						load = form.setup + string(script) + "\necho STILL-HERE"
					} else {
						load = form.setup + "eval " + shellQuoteForTest(string(script)) + "\necho STILL-HERE"
					}

					cmd := exec.Command(path, "-c", load)
					// CM_SESSION deliberately unset: this is a shell outside cm, which is the case being
					// checked.
					cmd.Env = append(os.Environ(), "CM_SESSION=")
					out, err := cmd.CombinedOutput()
					if err != nil {
						t.Fatalf("loading the %s integration outside a session failed: %v\n%s",
							shell, err, out)
					}
					if !strings.Contains(string(out), "STILL-HERE") {
						t.Errorf("%s stopped executing after loading the integration: %q", shell, out)
					}
					// Nothing but the marker: no error message, no stray escape sequence.
					if got := strings.TrimSpace(string(out)); got != "STILL-HERE" {
						t.Errorf("%s printed %q outside a session, want only the marker", shell, got)
					}
				})
			}
		})
	}
}

// shellQuoteForTest wraps s in single quotes for a POSIX-ish shell, escaping any it contains.
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
