package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// shelveEnv is a server that persists, runs a declared resume command on a revive when the program is sh,
// and starts its shells at a prompt that tells cm the line editor is up, so a typed resume command is not
// held for the 10s a revive otherwise waits.
//
// The prompt's HOME is passed to every client through the environment as well as to the first attach, since
// a restore carries the restoring client's environment.
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

// infoJSON is `cm info NAME --json` decoded.
func infoJSON(e *env, name string) map[string]any {
	e.t.Helper()
	var info map[string]any
	if err := json.Unmarshal([]byte(e.mustRun("info", name, "--json")), &info); err != nil {
		e.t.Fatalf("info --json is not JSON: %v", err)
	}
	return info
}
