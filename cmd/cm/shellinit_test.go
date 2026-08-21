package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/shellinit"
)

// zshCompinit initializes zsh's completion system ahead of loading cm's output.
//
// `-u` rather than a bare `compinit`: on a machine with a group-writable directory in fpath, compinit
// stops and asks via `read -q` whether to continue, answering itself with `n` when nothing is
// interactive, which aborts and unfunctions compdef. The completions half then prints
// `command not found: compdef` and the test reports cm as broken. `-u` accepts those directories without
// asking, which is what a test wants: the audit guards against another user's completion functions and
// nothing here asserts on fpath permissions.
//
// stderr is not redirected, on purpose. With `-u` compinit is silent, so anything printed from here on is
// a real finding rather than noise to mask. See the longer note in internal/e2e/shellinit_test.go, where
// this same prompt corrupted a pty's input and cost most of a CI run to diagnose.
const zshCompinit = "autoload -Uz compinit; compinit -u -D\n"

// runShellInit returns what `cm shell-init <shell> [args...]` writes to stdout.
//
// Driven through the real root command rather than by calling the generators directly, since the
// bundling this file is about happens in RunE and the completions come from the root's own commands.
func runShellInit(t *testing.T, args ...string) string {
	t.Helper()

	// stdout is where both halves are written, so the command is run with it redirected to a pipe
	// rather than captured through cobra's SetOut, which the cobra generators bypass.
	out, err := captureStdout(t, func() error {
		root := newRootCommand()
		root.SetArgs(append([]string{"shell-init"}, args...))
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		return root.Execute()
	})
	if err != nil {
		t.Fatalf("shell-init %v error = %v", args, err)
	}
	return out
}

// Completions come by default, so a startup file needs one cm invocation rather than two.
func TestShellInitBundlesCompletionsByDefault(t *testing.T) {
	for _, shell := range shellinit.Shells() {
		t.Run(shell, func(t *testing.T) {
			got := runShellInit(t, shell)

			script, err := shellinit.Script(shell)
			if err != nil {
				t.Fatalf("Script(%q) error = %v", shell, err)
			}
			var completions strings.Builder
			if err := writeCompletions(newRootCommand(), &completions, shell); err != nil {
				t.Fatalf("writeCompletions(%q) error = %v", shell, err)
			}

			// The completions come first: the integration is the part a reader is likely to be looking
			// for, and putting it last keeps it at the end of the output rather than buried.
			want := completions.String() + script
			if got != want {
				t.Errorf("shell-init %s output does not equal the completions followed by the integration\n"+
					"got %d bytes, want %d", shell, len(got), len(want))
			}
		})
	}
}

// --no-completions has to print the integration and nothing else, since that is the escape hatch for a
// startup file that cannot load completions at this point.
func TestShellInitNoCompletionsPrintsOnlyTheIntegration(t *testing.T) {
	for _, shell := range shellinit.Shells() {
		t.Run(shell, func(t *testing.T) {
			got := runShellInit(t, shell, "--no-completions")

			want, err := shellinit.Script(shell)
			if err != nil {
				t.Fatalf("Script(%q) error = %v", shell, err)
			}
			if got != want {
				t.Errorf("shell-init %s --no-completions = %q, want the integration alone", shell, got)
			}
		})
	}
}

// The bundled output must not stop the rest of the user's startup file from running.
//
// internal/shellinit pins this for the integration alone, and it has to be pinned again here because
// bundling puts a second script in front of it. A `return` at the top level of the completions would
// end the eval before the integration was ever defined, and in zsh it would take the rest of .zshrc
// with it: `zsh -c 'eval "return 0"; echo AFTER'` prints nothing.
//
// Loaded the way the help says to load it. Sourcing instead of evaluating hides the zsh failure.
func TestShellInitDoesNotAbortTheStartupFile(t *testing.T) {
	for _, shell := range shellinit.Shells() {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}

			script := runShellInit(t, shell)

			var load string
			switch shell {
			case "fish":
				// fish has no eval of a multi-line string; `| source` is what its help documents.
				load = script + "\necho STILL-HERE"
			case "zsh":
				// compinit first, since the completions half registers a completion function and this is
				// the ordering the help tells the user to use.
				load = zshCompinit +
					"eval " + shellSingleQuote(script) + "\necho STILL-HERE"
			default:
				load = "eval " + shellSingleQuote(script) + "\necho STILL-HERE"
			}

			cmd := exec.Command(bin, "-c", load)
			cmd.Env = append(cmd.Environ(), "CM_SESSION=test")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("loading the bundled %s output failed: %v\n%s", shell, err, out)
			}
			if !strings.Contains(string(out), "STILL-HERE") {
				t.Errorf("loading the bundled %s output stopped the rest of the startup file; output: %q",
					shell, out)
			}
		})
	}
}

// Both halves have to actually take effect together: the function defined and the completion
// registered. Asserting only that the bytes concatenate would pass if the completions half were
// syntactically broken by whatever precedes it.
func TestShellInitLoadsBothHalves(t *testing.T) {
	bin, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("zsh is not installed")
	}

	script := runShellInit(t, "zsh")
	load := zshCompinit +
		"eval " + shellSingleQuote(script) + "\n" +
		"typeset -f cm_report >/dev/null && echo REPORT-DEFINED\n" +
		"(( $+functions[_cm] )) && echo COMPLETION-DEFINED\n"

	cmd := exec.Command(bin, "-c", load)
	cmd.Env = append(cmd.Environ(), "CM_SESSION=test")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("loading the bundled zsh output failed: %v\n%s", err, out)
	}

	got := string(out)
	for _, want := range []string{"REPORT-DEFINED", "COMPLETION-DEFINED"} {
		if !strings.Contains(got, want) {
			t.Errorf("bundled zsh output did not produce %s; output: %q", want, got)
		}
	}
}

// An unsupported shell must fail without writing anything, or a shell eval'ing the output would load
// completions alongside the error.
func TestShellInitRejectsUnknownShellBeforeWriting(t *testing.T) {
	out, err := captureStdout(t, func() error {
		root := newRootCommand()
		root.SetArgs([]string{"shell-init", "tcsh"})
		root.SetOut(&strings.Builder{})
		root.SetErr(&strings.Builder{})
		return root.Execute()
	})
	if err == nil {
		t.Fatal("shell-init tcsh error = nil, want a failure naming the supported shells")
	}
	if out != "" {
		t.Errorf("shell-init tcsh wrote %q to stdout, want nothing", out)
	}
}

// shellSingleQuote wraps s in single quotes for a POSIX-ish shell, escaping any it contains.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
