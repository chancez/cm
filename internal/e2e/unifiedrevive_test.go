package e2e

import (
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
)

// endSession ends a persisted session the way state names: "exited" by `cm kill --keep`, which the shell
// answers, and "dead" by SIGKILLing its shim, which is what a crash or a reboot leaves: the server learns
// the stream ended and can get no status.
func endSession(e *env, name, state string) {
	e.t.Helper()
	switch state {
	case "exited":
		e.mustRun("kill", name, "--keep")
	case "dead":
		killShim(e, e.sessionID(name))
	default:
		e.t.Fatalf("endSession: no way to reach %q", state)
	}
	e.waitFor("the session to be "+state, 15*time.Second, func() bool {
		s, ok := e.session(name)
		return ok && s.State == state
	})
}

// killShim SIGKILLs the shim holding a session, matched on this environment's runtime directory and the
// session's ID so nothing another test or the developer started can be reached.
func killShim(e *env, id string) {
	e.t.Helper()
	out, err := exec.Command("ps", "ax", "-o", "pid=,command=").Output()
	if err != nil {
		e.t.Fatalf("ps: %v", err)
	}
	for line := range strings.Lines(string(out)) {
		if !strings.Contains(line, e.runtime) || !strings.Contains(line, " shim ") ||
			!strings.Contains(line, "--session "+id+" ") {
			continue
		}
		pid, err := strconv.Atoi(strings.Fields(line)[0])
		if err != nil {
			continue
		}
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
			e.t.Fatalf("kill %d: %v", pid, err)
		}
		return
	}
	e.t.Fatalf("no shim found for session %s", id)
}

// An attach with a command to an ended session that saved output brings it back as it was, however it ended:
// the declared resume command runs in the recorded directory, the attach's command does not, and the client
// says so on stderr.
//
// The dead case is the reboot: a terminal restoring its windows replays `cm attach NAME -- ARGV`, and the
// attach's command used to win there, in the client's directory, so the declared command was never used.
func TestAttachWithACommandRevivesAnEndedSessionAsItWas(t *testing.T) {
	skipIfShort(t)
	for _, state := range []string{"exited", "dead"} {
		t.Run(state, func(t *testing.T) {
			e := shelveEnv(t)
			startDeclared(e, "agent", filepath.Join(e.state, "recorded-dir"))
			endSession(e, "agent", state)

			r := e.run("attach", "--no-attach", "agent", "--", "/bin/sh", "-c", "echo WRONG-COMMAND; exec sleep 30")
			if r.code != 0 {
				t.Fatalf("attach exited %d: %s", r.code, r.stderr)
			}
			want := "cm: agent is " + state + " and comes back as it was; ignoring the command after --\n"
			if r.stderr != want {
				t.Errorf("attach stderr = %q, want %q", r.stderr, want)
			}
			history := e.waitForHistory("agent", "RESUMED-recorded-dir")
			if strings.Contains(history, "WRONG-COMMAND") {
				t.Errorf("the attach's own command ran; history:\n%s", history)
			}
			if s, _ := e.session("agent"); s.State != "running" {
				t.Errorf("after the revive, state %q, want running", s.State)
			}
			e.mustRun("kill", "agent")
		})
	}
}

// `cm run --session X` on an exited X keeps doing what it did before revives were unified: it reuses X in
// place, same ID, with the run's command as the session's command, prints X's saved output followed by the
// command's, and exits with the command's status. The declared resume command is not used, since a run must
// never drop its command.
//
// The whole stdout is compared, which is what pins the saved output being replayed ahead of the run's.
func TestRunReusesAnExitedSessionWithItsOwnCommand(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	e.mustRun("attach", "--no-attach", "--persist", "build", "--", "/bin/sh", "-c", "echo FIRST-RUN; exec sleep 300")
	e.waitForHistory("build", "FIRST-RUN")
	e.mustRun("restore-with", "build", "--", "sh", "-c", "echo RESUMED")
	id := e.sessionID("build")
	endSession(e, "build", "exited")

	r := e.run("run", "--session", "build", "--", "/bin/sh", "-c", "echo SECOND-RUN; exit 3")
	want := result{stdout: "FIRST-RUN\nSECOND-RUN\n", stderr: "cm: command exited with status 3\n", code: 3}
	if r != want {
		t.Errorf("cm run on an exited session = %+v, want %+v", r, want)
	}
	s, ok := e.session("build")
	if !ok || s.ID != id || s.State != "exited" || s.ExitCode != 3 {
		t.Errorf("after the run, session = %+v, want %s exited(3)", s, id)
	}
	e.mustRun("kill", "build")
}

// A session spawned as a program comes back as that program, even when something inside it wrote a command
// frame, the way a remote shell with cm's integration writes one through an ssh.
//
// The frame is printed by the spawned script itself, so the outermost frame the server sees is not from a
// local shell. Recorded as the typed command, its line would have been typed into a local shell on the
// revive, in place of re-running the script, which would then print SPAWNED only once.
func TestASessionThatIsAProgramRevivesItsSpawnArgv(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	enter := string(osc.FrameSequence("far-1", "remote-vim notes.md", false))
	e.mustRun("attach", "--no-attach", "--persist", "remote", "--",
		"/bin/sh", "-c", `printf '%s' "$1"; echo SPAWNED; exec sleep 300`, "sh", enter)
	e.waitForHistory("remote", "SPAWNED")
	if got := e.mustRun("info", "remote", "--field", "typed_command"); got != "\n" {
		t.Errorf("typed_command of a session that is a program = %q, want empty", got)
	}
	endSession(e, "remote", "exited")

	e.mustRun("attach", "--no-attach", "remote")
	e.waitForHistory("remote", "SPAWNED\nSPAWNED")
	e.mustRun("kill", "remote")
}

// A revive re-runs the spawn argv argument for argument, so an argument with a space in it is still one.
//
// The record used to hold the argv joined with spaces and a revive split it again on whitespace, so the
// quoted argument below came back as two and the script printed only its first half.
func TestReviveRerunsTheSpawnArgvExactly(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	e.mustRun("attach", "--no-attach", "--persist", "quoted", "--",
		"/bin/sh", "-c", `echo "ARG=[$1]"; exec sleep 300`, "sh", "a b")
	e.waitForHistory("quoted", "ARG=[a b]")
	endSession(e, "quoted", "exited")

	e.mustRun("attach", "--no-attach", "quoted")
	history := e.waitForHistory("quoted", "ARG=[a b]\nARG=[a b]")
	if strings.Contains(history, "ARG=[a]") {
		t.Errorf("the quoted argument was split on the revive; history:\n%s", history)
	}
	e.mustRun("kill", "quoted")
}
