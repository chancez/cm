package shellinit

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/osc"
)

// Every shipped script must be listed, and every listed script must exist.
func TestShellsMatchScripts(t *testing.T) {
	got := Shells()
	want := []string{"bash", "fish", "zsh"}

	if len(got) != len(want) {
		t.Fatalf("Shells() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Shells() = %v, want %v", got, want)
		}
	}
	for _, shell := range got {
		if _, err := Script(shell); err != nil {
			t.Errorf("Script(%q) error = %v", shell, err)
		}
	}
}

func TestScriptRejectsUnknownShell(t *testing.T) {
	_, err := Script("tcsh")
	if err == nil {
		t.Fatal("Script() error = nil, want an error naming the supported shells")
	}
	// The message has to say what is supported, or a caller has to guess.
	for _, shell := range Shells() {
		if !strings.Contains(err.Error(), shell) {
			t.Errorf("error %q does not mention %q", err, shell)
		}
	}
}

// Each script must emit the number the parser reads, since the two are one wire format split across two
// languages. A change to either alone breaks every installed integration silently.
func TestScriptsUseTheRightOSCNumber(t *testing.T) {
	for _, shell := range Shells() {
		script, err := Script(shell)
		if err != nil {
			t.Fatalf("Script(%q) error = %v", shell, err)
		}
		want := "\\033]25453;"
		if !strings.Contains(script, want) {
			t.Errorf("%s script does not emit %q (OSC %d)", shell, want, osc.ReportNumber)
		}
	}
}

// A script must do nothing outside a session, so it is safe to load unconditionally from an rc file.
func TestScriptsGuardOnSessionEnv(t *testing.T) {
	for _, shell := range Shells() {
		script, err := Script(shell)
		if err != nil {
			t.Fatalf("Script(%q) error = %v", shell, err)
		}
		if !strings.Contains(script, "CM_SESSION") {
			t.Errorf("%s script does not check CM_SESSION, so it would load outside a session", shell)
		}
	}
}

// The reports a real shell produces must parse, which is the property that matters and the one a Go test
// alone cannot check: the escaping bug this pins down lived entirely in the shell script.
//
// Run against the actual shell rather than a transcription of what it should emit. The first version
// interpolated the detail into printf's format string, so a literal backslash became a backspace byte and a
// percent sign consumed whatever followed it. Both round-tripped fine in a Go test of the parser, because
// the parser was never the broken half.
func TestRealShellsProduceParseableReports(t *testing.T) {
	// A detail exercising every character with meaning: the field separator, the escape character, and
	// printf's format specifier.
	const detail = `a;b\c 50% x`

	tests := []struct {
		shell string
		// call is the shell source that loads the script and emits one report to stdout.
		call string
	}{
		{
			shell: "zsh",
			call:  `d='a;b\c 50% x'; cm_report blocked "$d"`,
		},
		{
			shell: "bash",
			call:  `d='a;b\c 50% x'; cm_report blocked "$d"`,
		},
		{
			shell: "fish",
			call:  `set d 'a;b\c 50% x'; cm_report blocked $d`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			bin, err := exec.LookPath(tt.shell)
			if err != nil {
				t.Skipf("%s is not installed", tt.shell)
			}

			script, err := Script(tt.shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}

			// /dev/tty is where the script writes, since that is what reaches cm rather than a redirect.
			// There is no terminal here, so it is pointed at stdout for the duration.
			source := strings.Replace(script, "> /dev/tty 2>/dev/null", "", -1)

			cmd := exec.Command(bin, "-c", source+"\n"+tt.call)
			// The scripts return early unless this is set, which is the behavior TestScriptsGuardOnSessionEnv
			// covers; here it has to be set for anything to be emitted at all.
			cmd.Env = append(cmd.Environ(), "CM_SESSION=test")

			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("running the %s integration failed: %v\n%s", tt.shell, err, out)
			}

			var tr osc.ReportTracker
			if !tr.Feed(out) {
				t.Fatalf("%s emitted no parseable report: %q", tt.shell, out)
			}
			got, _ := tr.Take()
			want := osc.Report{State: "blocked", Detail: detail, Source: tt.shell}
			if got != want {
				t.Errorf("report = %+v, want %+v\nraw: %q", got, want, out)
			}
		})
	}
}

// A state with no detail must still parse, since that is the common call.
func TestRealShellsReportWithoutDetail(t *testing.T) {
	for _, shell := range Shells() {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			script, err := Script(shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}
			source := strings.Replace(script, "> /dev/tty 2>/dev/null", "", -1)

			cmd := exec.Command(bin, "-c", source+"\ncm_report idle")
			cmd.Env = append(cmd.Environ(), "CM_SESSION=test")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("running the %s integration failed: %v\n%s", shell, err, out)
			}

			var tr osc.ReportTracker
			if !tr.Feed(out) {
				t.Fatalf("%s emitted no parseable report: %q", shell, out)
			}
			got, _ := tr.Take()
			want := osc.Report{State: "idle", Source: shell}
			if got != want {
				t.Errorf("report = %+v, want %+v\nraw: %q", got, want, out)
			}
		})
	}
}

// Loading a script outside a session must emit nothing, which is what makes it safe in an rc file.
func TestRealShellsAreInertOutsideASession(t *testing.T) {
	for _, shell := range Shells() {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			script, err := Script(shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}

			cmd := exec.Command(bin, "-c", script)
			// CM_SESSION deliberately absent, which is the case being checked.
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("loading the %s integration outside a session failed: %v\n%s", shell, err, out)
			}
			if len(out) != 0 {
				t.Errorf("%s emitted %q outside a session, want nothing", shell, out)
			}
		})
	}
}

// Loading the integration must not stop the rest of the user's startup file from running.
//
// This pins the worst bug these scripts had, and it is deliberately separate from the inert check above.
// Each script originally bailed out early with `return` (or `exit` for fish), which reads as obviously
// correct and is wrong in a different way in every shell:
//
//   - zsh: a `return` inside `eval` returns from the *enclosing* scope, so everything after the eval in
//     .zshrc is silently skipped. Verified directly: `zsh -c 'eval "return 0"; echo AFTER'` prints nothing.
//   - bash: refuses a `return` outside a sourced file, printing an error on every shell startup.
//   - fish: `exit` in a script piped to `source` would end the user's shell.
//
// The zsh case is why the inert check cannot stand in for this one: an early return produces no output at
// all, so a test asserting silence passes while the bug is present. Only checking that *later* lines still
// run distinguishes them.
//
// Loaded the way the help says to load it, since sourcing instead of evaluating hides the zsh failure
// entirely. Checked both in and out of a session, because the guard runs in one case and the body in the
// other.
func TestRealShellsDoNotAbortTheStartupFile(t *testing.T) {
	for _, shell := range Shells() {
		for _, inSession := range []bool{false, true} {
			name := shell + "/outside-a-session"
			if inSession {
				name = shell + "/in-a-session"
			}
			t.Run(name, func(t *testing.T) {
				bin, err := exec.LookPath(shell)
				if err != nil {
					t.Skipf("%s is not installed", shell)
				}
				script, err := Script(shell)
				if err != nil {
					t.Fatalf("Script() error = %v", err)
				}

				var load string
				switch shell {
				case "fish":
					// fish has no eval of a multi-line string; `| source` is what its help documents.
					load = script + "\necho STILL-HERE"
				default:
					load = "eval " + shellQuote(script) + "\necho STILL-HERE"
				}

				cmd := exec.Command(bin, "-c", load)
				cmd.Env = cmd.Environ()
				if inSession {
					cmd.Env = append(cmd.Env, "CM_SESSION=test")
				}
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("loading the %s integration failed: %v\n%s", shell, err, out)
				}
				if !strings.Contains(string(out), "STILL-HERE") {
					t.Errorf("loading the %s integration stopped the rest of the startup file; output: %q",
						shell, out)
				}
			})
		}
	}
}

// shellQuote wraps s in single quotes for a POSIX-ish shell, escaping any it contains.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
