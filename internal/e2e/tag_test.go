package e2e

import (
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Tagging at creation and filtering on it must work through the real binary.
//
// Worth an end-to-end test rather than only unit ones because the wiring is where this can break: the
// flag has to reach the Open message, the server has to store it, and the selector has to survive the
// round trip. Every one of those is a separate layer that a unit test covers alone.
func TestTagsAtCreationAndFiltering(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "one", "--tag", "project=cm", "--tag", "role=reviewer",
		"-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "two", "--tag", "project=cm", "--tag", "role=builder",
		"-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "three", "--tag", "project=zmx",
		"-d", "--", "/bin/sh", "-c", "sleep 120")
	e.mustRun("run", "--session", "plain", "-d", "--", "/bin/sh", "-c", "sleep 120")

	// The whole tag set on one session, so the map arrives intact rather than one key surviving.
	s, ok := e.session("one")
	if !ok {
		t.Fatal("session one is missing")
	}
	wantTags := map[string]string{"project": "cm", "role": "reviewer"}
	if !reflect.DeepEqual(s.Tags, wantTags) {
		t.Errorf("session one tags = %v, want %v", s.Tags, wantTags)
	}

	tests := []struct {
		name      string
		selectors []string
		want      []string
	}{
		{name: "by value", selectors: []string{"project=cm"}, want: []string{"one", "two"}},
		{name: "bare key", selectors: []string{"role"}, want: []string{"one", "two"}},
		// Repeating narrows rather than widens, which is the part a user could reasonably expect to
		// work the other way.
		{
			name:      "two terms",
			selectors: []string{"project=cm", "role=builder"},
			want:      []string{"two"},
		},
		{name: "no matches", selectors: []string{"project=absent"}, want: nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.tagNames(tc.selectors...); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("list --tag %v = %v, want %v", tc.selectors, got, tc.want)
			}
		})
	}

	// An untagged session is reachable only without a selector, which is what proves the filter is
	// filtering rather than the tags being ignored.
	if _, ok := e.session("plain"); !ok {
		t.Error("the untagged session is missing from an unfiltered list")
	}
}

// Every command that can create a session must apply --tag.
//
// This caught a real bug: `attach --no-attach` builds its own Open message rather than going through
// the client's, so it silently dropped the tags while `run` and a plain `attach` kept them. There are
// three hand-built Open messages, and nothing makes them agree, so a field added to one is missing from
// the others until something checks. A tag set on creation and never seen again is the kind of failure
// that looks like the user's mistake.
func TestEveryCreationPathAppliesTags(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "run",
			args: []string{"run", "--session", "via-run", "--tag", "path=run",
				"-d", "--", "/bin/sh", "-c", "sleep 120"},
		},
		{
			// The path that was broken.
			name: "attach --no-attach",
			args: []string{"attach", "--no-attach", "via-attach", "--tag", "path=attach",
				"--", "/bin/sh", "-c", "sleep 120"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e.mustRun(tc.args...)
		})
	}

	// Asserted by listing rather than per-session, so a path that stored nothing shows up as a missing
	// row instead of an empty map that could be mistaken for something else.
	//
	// Sorted before comparing: `list` orders by creation time and breaks ties by name, and these are
	// created in the same second, so the order depends on the names rather than on the argument order
	// here.
	got := e.tagNames("path")
	sort.Strings(got)
	if want := []string{"via-attach", "via-run"}; !reflect.DeepEqual(got, want) {
		t.Errorf("list --tag path = %v, want both creation paths tagged: %v", got, want)
	}

	for _, name := range []string{"via-run", "via-attach"} {
		s, ok := e.session(name)
		if !ok {
			t.Fatalf("session %s is missing", name)
		}
		if len(s.Tags) == 0 {
			t.Errorf("session %s has no tags, want the ones it was created with", name)
		}
	}
}

// `cm tag` changes tags on a running session, and removal persists.
func TestTagCommandSetsAndRemoves(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "work", "-d", "--", "/bin/sh", "-c", "sleep 120")

	// Tagging a session that was created without any.
	e.mustRun("tag", "work", "project=cm", "role=reviewer")
	s, _ := e.session("work")
	want := map[string]string{"project": "cm", "role": "reviewer"}
	if !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("tags after tagging = %v, want %v", s.Tags, want)
	}

	// Merging by default: a second call must not discard the first one's work.
	e.mustRun("tag", "work", "extra=1")
	s, _ = e.session("work")
	want = map[string]string{"project": "cm", "role": "reviewer", "extra": "1"}
	if !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("tags after a second call = %v, want them merged: %v", s.Tags, want)
	}

	e.mustRun("tag", "work", "--remove", "role")
	s, _ = e.session("work")
	want = map[string]string{"project": "cm", "extra": "1"}
	if !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("tags after --remove = %v, want %v", s.Tags, want)
	}

	// --replace defines the whole set.
	e.mustRun("tag", "work", "--replace", "fresh=1")
	s, _ = e.session("work")
	want = map[string]string{"fresh": "1"}
	if !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("tags after --replace = %v, want only the given set: %v", s.Tags, want)
	}

	// Removing the last tag leaves none. The store treats an empty set as "clear", so this is where
	// a nil map would silently mean "leave them alone" and the tag would survive its own removal.
	e.mustRun("tag", "work", "--remove", "fresh")
	s, _ = e.session("work")
	if len(s.Tags) != 0 {
		t.Errorf("tags after removing the last one = %v, want none", s.Tags)
	}
}

// A tag with an escape sequence must be refused rather than stored.
//
// The failure this prevents is not hypothetical: tags are printed by `cm list`, so a stored escape
// sequence would run in the terminal of whoever lists sessions. Checked through the binary because
// that is the path a user takes.
func TestTagRejectsEscapeSequences(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "work", "-d", "--", "/bin/sh", "-c", "sleep 120")

	for _, bad := range []string{
		"k=\x1b]2;pwned\x07",           // OSC, retitles the window
		"k=\x1b[2J",                    // CSI, clears the screen
		"k=a b",                        // a space, which would break the column
		"k=" + strings.Repeat("a", 64), // over the 63-byte limit
	} {
		r := e.run("tag", "work", bad)
		if r.code == 0 {
			t.Errorf("tag %q exited 0, want it refused", bad)
		}
	}

	// Nothing was stored by any refused call.
	s, _ := e.session("work")
	if len(s.Tags) != 0 {
		t.Errorf("tags = %v, want none after every call was refused", s.Tags)
	}

	// And the same at creation time, which goes through a different code path: the Open message
	// rather than the Tag RPC.
	if r := e.run("run", "--session", "hostile", "--tag", "k=\x1b]2;pwned\x07",
		"-d", "--", "/bin/sh", "-c", "sleep 120"); r.code == 0 {
		t.Error("run --tag with an escape sequence exited 0, want it refused")
	}
}

// Tags survive a server restart, which is what makes them different from a report.
//
// A report is held in memory and deliberately does not survive, since it describes a running program.
// A tag describes the session, so it has to come back.
func TestTagsSurviveAServerRestart(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "work", "--tag", "project=cm",
		"-d", "--", "/bin/sh", "-c", "sleep 120")
	e.restartServer()

	s, ok := e.session("work")
	if !ok {
		t.Fatal("the session did not survive the restart")
	}
	want := map[string]string{"project": "cm"}
	if !reflect.DeepEqual(s.Tags, want) {
		t.Errorf("tags after a restart = %v, want %v", s.Tags, want)
	}
	// And still filterable, since the selector reads what was reloaded rather than a cache.
	if got := e.tagNames("project=cm"); !reflect.DeepEqual(got, []string{"work"}) {
		t.Errorf("list --tag after a restart = %v, want [work]", got)
	}
}
