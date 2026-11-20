package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// shelveEnv is a server that persists, runs a declared resume command on a revive when the program is sh,
// and starts its shells at a prompt that tells cm the line editor is up, so a typed resume command is not
// held for the 10s a revive otherwise waits.
//
// The prompt's HOME is passed to every client through the environment as well as to the first attach, since
// a restore carries the restoring client's environment and `cm restore` has no --env.
func shelveEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t)
	e.writeConfig(`
[persist]
enabled = true
on_restore = "command"
safe_commands = ["sh"]
`)
	// Restarted onto the config, which a server reads when it starts and newEnv has already started one.
	e.restartServer()
	home := e.markedPromptEnv()[1]
	e.extraEnv = append(e.extraEnv, home)
	return e
}

// startDeclared creates a persisted session in dir and declares a resume command that prints where it ran.
//
// The command prints the name of its working directory, so a revive's directory is asserted by output rather
// than by what cm recorded, and its output differs from the typed line so the echo cannot satisfy a wait. The
// name rather than the path, which at 90 columns of temp directory wraps in an 80-column history; the tests
// give each directory a name the client's own, "s", cannot be mistaken for.
func startDeclared(e *env, name, dir string) {
	e.t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	e.mustRun(append([]string{"attach", "--no-attach", "--persist", "--dir", dir, name}, e.markedPromptEnv()...)...)
	e.waitForHistory(name, promptMarker)
	e.mustRun("restore-with", name, "--", "sh", "-c", `echo "RESUMED-$(basename "$(pwd)")"`)
}

// `cm shelve` ends a running session and lists it as shelved with nothing set to expire it, and a
// second `cm shelve`, or one of a session that saved nothing, says so.
func TestShelveListsTheSessionAsShelved(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	startDeclared(e, "agent", filepath.Join(e.state, "recorded-dir"))
	if got := e.mustRun("shelve", "agent"); got != "shelved agent\n" {
		t.Errorf("cm shelve printed %q, want %q", got, "shelved agent\n")
	}
	// The fields shelving decides, together: the state, the retention a restore clears, and the declaration
	// it comes back with.
	info := infoJSON(e, "agent")
	got := map[string]any{"state": info["state"], "expire_after": info["expire_after"], "resume": info["resume"]}
	want := map[string]any{
		"state": "shelved", "expire_after": "never",
		"resume": []any{"sh", "-c", `echo "RESUMED-$(basename "$(pwd)")"`},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("info after cm shelve = %v, want %v", got, want)
	}

	if got := e.mustRun("shelve", "agent"); got != "agent is already shelved\n" {
		t.Errorf("second cm shelve printed %q, want %q", got, "agent is already shelved\n")
	}

	e.mustRun("attach", "--no-attach", "plain")
	r := e.run("shelve", "plain")
	if r.code == 0 || !strings.Contains(r.stderr, "plain: saved no output, so there is nothing to restore it to") {
		t.Errorf("cm shelve of an unpersisted session = exit %d, stderr %q; want a refusal", r.code, r.stderr)
	}
	e.mustRun("kill", "agent")
	e.mustRun("kill", "plain")
}

// An attach naming a command restores a shelved session as it was: the declared resume command runs, not the
// one given, in the recorded directory rather than the client's, and the client says on stderr that it
// ignored the command.
//
// --no-attach, which is a restore, so the run is deterministic; the server path is the one an interactive
// attach takes, through the same Open.
func TestAttachWithACommandRestoresAShelvedSessionAsItWas(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	project := filepath.Join(e.state, "recorded-dir")
	startDeclared(e, "agent", project)
	e.mustRun("shelve", "agent", "--expire-after", "720h")

	r := e.run("attach", "--no-attach", "agent", "--", "/bin/sh", "-c", "echo WRONG-COMMAND; exec sleep 30")
	if r.code != 0 {
		t.Fatalf("attach exited %d: %s", r.code, r.stderr)
	}
	if want := "cm: agent is shelved and comes back as it was; ignoring the command after --\n"; r.stderr != want {
		t.Errorf("attach stderr = %q, want %q", r.stderr, want)
	}
	history := e.waitForHistory("agent", "RESUMED-recorded-dir")
	if strings.Contains(history, "WRONG-COMMAND") {
		t.Errorf("the attach's own command ran; history:\n%s", history)
	}
	if info := infoJSON(e, "agent"); info["state"] != "running" || info["expire_after"] != "" {
		t.Errorf("after the restore, state %v and expire_after %v; want running and empty",
			info["state"], info["expire_after"])
	}
	e.mustRun("kill", "agent")
}

// An explicit --dir is a request rather than the client's default, so a restore honors it.
func TestRestoringWithAnExplicitDirStartsThere(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	startDeclared(e, "agent", filepath.Join(e.state, "recorded-dir"))
	e.mustRun("shelve", "agent")

	elsewhere := filepath.Join(e.state, "elsewhere-dir")
	if err := os.MkdirAll(elsewhere, 0o700); err != nil {
		t.Fatal(err)
	}
	e.mustRun("attach", "--no-attach", "--dir", elsewhere, "agent")
	e.waitForHistory("agent", "RESUMED-elsewhere-dir")
	e.mustRun("kill", "agent")
}

// `cm restore` starts a shelved session without attaching, reports a running one as nothing to do, and
// refuses one that saved no output.
func TestRestoreStartsAShelvedSessionWithoutAttaching(t *testing.T) {
	skipIfShort(t)
	e := shelveEnv(t)
	project := filepath.Join(e.state, "recorded-dir")
	startDeclared(e, "agent", project)
	e.mustRun("shelve", "agent")

	if got := e.mustRun("restore", "agent"); got != "restored agent\n" {
		t.Errorf("cm restore printed %q, want %q", got, "restored agent\n")
	}
	e.waitForHistory("agent", "RESUMED-recorded-dir")
	if s, _ := e.session("agent"); s.State != "running" || s.Clients != 0 {
		t.Errorf("after cm restore, state %q with %d clients; want running with none", s.State, s.Clients)
	}
	if got := e.mustRun("restore", "agent"); got != "agent is already running\n" {
		t.Errorf("second cm restore printed %q, want %q", got, "agent is already running\n")
	}

	e.mustRun("kill", "agent")

	// Nothing saved, so nothing to restore it to.
	e.mustRun("attach", "--no-attach", "plain")
	e.mustRun("kill", "plain", "--keep")
	e.waitFor("the session to have ended", 15*time.Second, func() bool {
		s, ok := e.session("plain")
		return ok && s.State != "running"
	})
	r := e.run("restore", "plain")
	if r.code == 0 || !strings.Contains(r.stderr, "plain: saved no output, so there is nothing to restore it to") {
		t.Errorf("cm restore of an unpersisted session = exit %d, stderr %q; want a refusal", r.code, r.stderr)
	}
	e.mustRun("kill", "plain")
}

// infoJSON is `cm info NAME --json` decoded.
func infoJSON(e *env, name string) map[string]any {
	e.t.Helper()
	var info map[string]any
	if err := json.Unmarshal([]byte(e.mustRun("info", name, "--json")), &info); err != nil {
		e.t.Fatalf("info --json is not JSON: %v", err)
	}
	return info
}
