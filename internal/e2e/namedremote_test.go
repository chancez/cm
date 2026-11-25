package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSSH writes an ssh stand-in that reaches far: it drops ssh's options and host, everything up to the
// "--" the proxy command puts before the remote's argv, and runs the rest against far's directories. So the
// "other machine" is a second isolated cm, and the transport is the real proxy over stdio.
func fakeSSH(t *testing.T, far *env) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cmssh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "ssh")
	script := fmt.Sprintf(`#!/bin/sh
while [ "$#" -gt 0 ] && [ "$1" != "--" ]; do shift; done
shift
CM_RUNTIME_DIR=%s CM_STATE_DIR=%s CM_CONFIG=/nonexistent.toml exec "$@"
`, far.runtime, far.state)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// completed returns what cobra's completion offers for args, one line each, without its trailing notes.
func completed(t *testing.T, e *env, args ...string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(e.mustRun(append([]string{"__complete"}, args...)...), "\n") {
		if line == "" || strings.HasPrefix(line, "Completion ended") {
			continue
		}
		out = append(out, line)
	}
	return out
}

// listNames returns the names of the sessions one invocation lists.
func listNames(t *testing.T, e *env, args ...string) []string {
	t.Helper()
	var listed []sessionJSON
	if err := json.Unmarshal([]byte(e.mustRun(append(args, "list", "--json")...)), &listed); err != nil {
		t.Fatalf("parsing list --json: %v", err)
	}
	var names []string
	for _, s := range listed {
		names = append(names, s.Name)
	}
	return names
}

// A qualified reference reaches the named host's server through the same transport --remote uses, and one
// qualified with this machine's own name stays here.
//
// End to end because the routing happens before cobra validates arguments, and a validator that refuses ':'
// first is invisible to a unit test that calls the router directly.
func TestQualifiedReferencesReachTheirHost(t *testing.T) {
	skipIfShort(t)
	far := newEnv(t)
	e := newEnv(t)
	e.writeConfig(fmt.Sprintf(`
[remote]
local_name = "here"

[remote.hosts.far]
address = "ssh://far%s"
ssh_command = "%s"
connection_persist = "0"
`, e.bin, fakeSSH(t, far)))

	e.mustRun("run", "--session", "far:job", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "here:mine", "-d", "--", "/bin/sh", "-c", "sleep 120")

	if got, want := listNames(t, far), []string{"job"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("far lists %q, want %q", got, want)
	}
	if got, want := listNames(t, e), []string{"mine"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("here lists %q, want %q", got, want)
	}
	// --remote by name reaches the same server.
	if got, want := listNames(t, e, "--remote", "far"), []string{"job"}; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("--remote far lists %q, want %q", got, want)
	}

	// By ID, which is how a window stores one.
	id := strings.TrimSpace(e.mustRun("--remote", "far", "info", "job", "--field", "id"))
	if got := strings.TrimSpace(e.mustRun("info", "far:"+id, "--field", "name")); got != "job" {
		t.Errorf("info far:%s name = %q, want job", id, got)
	}
	// The same reference qualified with this machine looks here, where there is no such session.
	if r := e.run("info", "here:"+id); r.code == 0 {
		t.Errorf("info here:%s succeeded, want it to look on this machine: %s", id, r.stdout)
	}
	if r := e.run("info", "nowhere:job"); r.code == 0 || !strings.Contains(r.stderr, `names host "nowhere"`) {
		t.Errorf("info nowhere:job = %d %q, want a refusal naming the host", r.code, r.stderr)
	}

	// Completion offers the host, with no space after it since a host alone is not a reference, and then the
	// host's own sessions, qualified as routing will read them. cobra prints each candidate on a line and the
	// directive last, as ":N": 4 is no file completion, 2 no trailing space.
	if got, want := completed(t, e, "attach", "fa"), []string{"far:\tsessions on ssh://far" + e.bin, ":6"}; !slices.Equal(got, want) {
		t.Errorf("complete attach fa = %q, want %q", got, want)
	}
	if got := completed(t, e, "attach", "far:"); len(got) != 2 || !strings.HasPrefix(got[0], "far:job\t") || got[1] != ":4" {
		t.Errorf("complete attach far: = %q, want far:job and :4", got)
	}
	if got := completed(t, e, "attach", "here:"); len(got) != 2 || !strings.HasPrefix(got[0], "here:mine\t") {
		t.Errorf("complete attach here: = %q, want here:mine", got)
	}

	e.mustRun("kill", "far:job")
	if got := listNames(t, far); len(got) != 0 {
		t.Errorf("far lists %q after kill far:job, want nothing", got)
	}
}
