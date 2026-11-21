package e2e

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"
)

// spacesOf maps each session's name to its space.
func (e *env) spacesOf() map[string]string {
	e.t.Helper()
	out := map[string]string{}
	for _, s := range e.list() {
		out[s.Name] = s.Space
	}
	return out
}

// A session lands in the space it was asked for, else in the space of the session it was created from,
// else in default, through both commands that create one.
//
// End to end because the unit tests stop at the options: whether the CLI sends the calling session at
// all is decided in cmd/cm, and attach is the case that matters, since it sends inside_session only on a
// terminal and a script's --no-attach has none.
func TestSpacesFollowCreation(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "parent", "--space", "infra", "-d", "--", "/bin/sh", "-c", "sleep 120")
	if r := e.runInSession("parent", "run", "--session", "by-run", "-d", "--", "/bin/sh", "-c", "sleep 120"); r.code != 0 {
		t.Fatalf("run inside parent: %s%s", r.stdout, r.stderr)
	}
	if r := e.runInSession("parent", "attach", "--no-attach", "by-attach"); r.code != 0 {
		t.Fatalf("attach --no-attach inside parent: %s%s", r.stdout, r.stderr)
	}
	// Asking beats inheriting.
	if r := e.runInSession("parent", "run", "--session", "asked", "--space", "cm", "-d", "--", "/bin/sh", "-c", "sleep 120"); r.code != 0 {
		t.Fatalf("run --space inside parent: %s%s", r.stdout, r.stderr)
	}
	e.mustRun("run", "--session", "outside", "-d", "--", "/bin/sh", "-c", "sleep 120")

	want := map[string]string{
		"parent":    "infra",
		"by-run":    "infra",
		"by-attach": "infra",
		"asked":     "cm",
		"outside":   "default",
	}
	if got := e.spacesOf(); !reflect.DeepEqual(got, want) {
		t.Errorf("spaces = %v, want %v", got, want)
	}

	var names []string
	var listed []sessionJSON
	if err := json.Unmarshal([]byte(e.mustRun("list", "--space", "infra", "--json")), &listed); err != nil {
		t.Fatalf("parsing list --space: %v", err)
	}
	for _, s := range listed {
		names = append(names, s.Name)
	}
	sort.Strings(names)
	if want := []string{"by-attach", "by-run", "parent"}; !reflect.DeepEqual(names, want) {
		t.Errorf("list --space infra = %v, want %v", names, want)
	}
}

// Moving a session is one command, the session calling it included, and a space holding a session
// refuses removal until it is empty.
func TestSpaceMoveAndRemove(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)
	e.mustRun("run", "--session", "a", "--space", "infra", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "b", "--space", "infra", "-d", "--", "/bin/sh", "-c", "sleep 120")

	if r := e.run("space", "remove", "infra"); r.code == 0 {
		t.Errorf("space remove of a space with sessions exited 0: %s", r.stdout)
	}

	e.mustRun("space", "move", "cm", "a")
	// With no session named, the one it runs in.
	if r := e.runInSession("b", "space", "move", "cm"); r.code != 0 {
		t.Fatalf("space move inside b: %s%s", r.stdout, r.stderr)
	}
	if got, want := e.spacesOf(), map[string]string{"a": "cm", "b": "cm"}; !reflect.DeepEqual(got, want) {
		t.Errorf("spaces after moving = %v, want %v", got, want)
	}

	var spaces []struct {
		Name     string `json:"name"`
		Sessions int    `json:"sessions"`
		Running  int    `json:"running"`
	}
	if err := json.Unmarshal([]byte(e.mustRun("space", "list", "--json")), &spaces); err != nil {
		t.Fatalf("parsing space list: %v", err)
	}
	type row struct {
		Name     string
		Sessions int
		Running  int
	}
	var got []row
	for _, sp := range spaces {
		got = append(got, row{sp.Name, sp.Sessions, sp.Running})
	}
	// infra stays, empty: a space outlives its sessions.
	want := []row{{"cm", 2, 2}, {"default", 0, 0}, {"infra", 0, 0}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("space list = %v, want %v", got, want)
	}

	e.mustRun("space", "remove", "infra")
}
