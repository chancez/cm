package tui

import (
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// sessionNamed builds a running session that answers to several names.
//
// Name is what the server would put there, which is the label rather than the set: internal/server.Label
// returns the first name. A fixture that filled it with the joined set would pass a row that reads Name
// instead of Names, which is the bug this file is about.
func sessionNamed(id string, names ...string) *serverv1.Session {
	return &serverv1.Session{
		Name:          names[0],
		Id:            id,
		Names:         names,
		State:         serverv1.SessionState_SESSION_STATE_RUNNING,
		CreatedAtUnix: 100,
	}
}

// A row shows every name a session answers to.
//
// The picker is where a session gets found, and the filter already searches all of them. Drawing only the
// label meant a row for an implicit per-window session read "kitty.325" while the name the user chose for
// it, and typed into the filter to get there, was nowhere on screen.
func TestARowShowsEveryName(t *testing.T) {
	h := newHarness(t, sessionNamed("a7k2m9x4", "kitty.325", "refactor"))
	h.list()

	if got := h.model.View().Content; !strings.Contains(got, "kitty.325,refactor") {
		t.Errorf("view = %q, want both names on the row", got)
	}
}

// A session nothing names still fills the cell.
//
// Name carries an ID reference in that case, because the server fills it with internal/server.Label. The
// cell must not go blank: every row is something a person is about to act on, and a row with no handle on
// it cannot be typed back into a command.
func TestARowNamesASessionWithNoBindings(t *testing.T) {
	s := sessionNamed("a7k2m9x4", "@a7k2m9x4")
	s.Names = nil
	h := newHarness(t, s)
	h.list()

	if got := h.model.View().Content; !strings.Contains(got, "@a7k2m9x4") {
		t.Errorf("view = %q, want the ID reference on the row", got)
	}
}

// Each ended state reads as itself, and a shelved session is not shown as exited.
//
// The server sets the exited flag on a shelved session as well, for older clients, so a row that checked
// the flag before the state would call a session ended on purpose to be restored later "exited(0)".
func TestARowSaysWhichWayASessionEnded(t *testing.T) {
	var got []string
	for _, state := range []serverv1.SessionState{
		serverv1.SessionState_SESSION_STATE_EXITED,
		serverv1.SessionState_SESSION_STATE_DEAD,
		serverv1.SessionState_SESSION_STATE_SHELVED,
	} {
		s := sessionNamed("a7k2m9x4", "work")
		s.State, s.Exited, s.ExitCode = state, true, 3
		got = append(got, item{session: s}.state())
	}
	if want := []string{"exited(3)", "dead", "shelved"}; !reflect.DeepEqual(got, want) {
		t.Errorf("state() = %q, want %q", got, want)
	}
}

// The name cell drops whole names when a set does not fit, never part of one.
//
// A reader is expected to type what the cell shows, so "kitty.325,refac" would be a name that resolves to
// nothing. The count is what says something was left out.
func TestARowElidesWholeNames(t *testing.T) {
	h := newHarness(t, sessionNamed("a7k2m9x4",
		"kitty.325", "refactor-the-store", "review", "deploy"))
	h.list()

	got := h.model.View().Content
	if !strings.Contains(got, "kitty.325 +3") {
		t.Errorf("view = %q, want the names that do not fit replaced by a count", got)
	}
	if strings.Contains(got, "refactor-the-st") {
		t.Errorf("view = %q, want no name cut in half", got)
	}
}

// A row shows the title a program set, and leaves out a title that only names the directory.
//
// The title is how a running program says what it is doing: claude titles its window with the task and
// gr with the PR, where the command reads "claude --resume <uuid>" and the list showed nothing better. A
// shell at its prompt titles itself with the directory the row already shows, in whatever spelling its
// prompt uses, and printing that is the same path twice.
func TestARowShowsTheProgramTitle(t *testing.T) {
	const home = "/home/u"
	cases := []struct {
		name    string
		session *serverv1.Session
		want    string
	}{{
		name: "a program's title replaces its command",
		session: &serverv1.Session{
			Cwd: "/home/u/docs", CwdIsLocal: true, Busy: true,
			Command: "claude --resume 98c5c1d5", Title: "Release notes for 10.0.1.0",
		},
		want: "Release notes for 10.0.1.0  ~/docs",
	}, {
		name: "an adopted session reports no command, and its title still shows",
		session: &serverv1.Session{
			Cwd: "/home/u/p/gr", CwdIsLocal: true, Title: "gr #5 pr/chancez/popup",
		},
		want: "gr #5 pr/chancez/popup  ~/p/gr",
	}, {
		name: "a reported detail still shows beside the title",
		session: &serverv1.Session{
			Cwd: "/home/u/docs", CwdIsLocal: true, Command: "claude",
			Title: "Release notes", ReportedDetail: "needs approval",
		},
		want: "Release notes  ~/docs  needs approval",
	}, {
		name: "a prompt's shortened directory is left out",
		session: &serverv1.Session{
			Cwd: "/home/u/projects/cm/.worktrees/tui", CwdIsLocal: true,
			Title: "~/p/c/.w/tui (pr/chancez/tui)",
		},
		want: "~/projects/cm/.worktrees/tui",
	}, {
		name: "a prompt's absolute directory is left out",
		session: &serverv1.Session{
			Cwd: "/srv/app", CwdIsLocal: true, Command: "make", Busy: true, Title: "/srv/app",
		},
		want: "/srv/app  make",
	}, {
		name:    "home itself is left out",
		session: &serverv1.Session{Cwd: "/home/u", CwdIsLocal: true, Title: "~"},
		want:    "~",
	}, {
		name: "another directory is not this one",
		session: &serverv1.Session{
			Cwd: "/home/u/docs", CwdIsLocal: true, Title: "~/dotfiles (master)",
		},
		want: "~/dotfiles (master)  ~/docs",
	}, {
		name:    "a deeper directory is not this one",
		session: &serverv1.Session{Cwd: "/srv", CwdIsLocal: true, Title: "/srv/app"},
		want:    "/srv/app  /srv",
	}, {
		name: "a remote shell's title takes the directory's place",
		session: &serverv1.Session{
			Cwd: "/home/r/app", CwdIsLocal: false, Command: "make", Title: "~/app (main)",
		},
		want: "~/app (main)  make",
	}, {
		name:    "no title leaves the command",
		session: &serverv1.Session{Cwd: "/home/u/docs", CwdIsLocal: true, Command: "make test"},
		want:    "~/docs  make test",
	}, {
		name:    "a space other than default leads",
		session: &serverv1.Session{Cwd: "/home/u/docs", CwdIsLocal: true, Space: "infra"},
		want:    "[infra]  ~/docs",
	}, {
		name:    "default is not repeated on every row",
		session: &serverv1.Session{Cwd: "/home/u/docs", CwdIsLocal: true, Space: "default"},
		want:    "~/docs",
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := (delegate{home: home}).tail(item{session: tc.session}); got != tc.want {
				t.Errorf("tail = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestAStartingFilterShowsWhatMatchReturns is the contract between `cm search` and `cm search --tui`: the
// command prints Match, the picker opens on its own filter, and the two must be the same sessions in the
// same order.
func TestAStartingFilterShowsWhatMatchReturns(t *testing.T) {
	notes := session("notes", "c9m4p1z6", 300)
	notes.Cwd = "/src/work"
	sessions := []*serverv1.Session{
		session("other", "b8l3n0y5", 200),
		notes,
		session("work", "a7k2m9x4", 100),
	}

	matched := Match("work", sessions)
	var got []string
	for _, s := range matched {
		got = append(got, s.Name)
	}
	// work outranks notes, whose match is in its directory, and other does not match.
	if want := []string{"work", "notes"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Match = %q, want %q", got, want)
	}

	h := newHarnessWith(t, Options{Filter: "work"}, sessions...)
	h.list()
	var shown []*serverv1.Session
	for _, li := range h.model.list.VisibleItems() {
		shown = append(shown, li.(item).session)
	}
	if !reflect.DeepEqual(shown, matched) {
		t.Errorf("the picker shows %v, want what Match returned, %v", shown, matched)
	}
	if got := h.model.list.FilterState(); got != list.FilterApplied {
		t.Errorf("filter state %v, want applied so the keys act on the list", got)
	}
}

// Rows group by space, default first, and keep creation order within one, as `cm ls` does.
func TestRowsGroupBySpace(t *testing.T) {
	sessions := []*serverv1.Session{
		{Name: "infra-old", Space: "infra", CreatedAtUnix: 1},
		{Name: "default-new", Space: "default", CreatedAtUnix: 9},
		{Name: "cm", Space: "cm", CreatedAtUnix: 5},
		{Name: "infra-new", Space: "infra", CreatedAtUnix: 8},
		{Name: "default-old", Space: "default", CreatedAtUnix: 2},
	}
	var got []string
	for _, it := range rows(sessions) {
		got = append(got, it.(item).session.Name)
	}
	want := []string{"default-old", "default-new", "cm", "infra-old", "infra-new"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v", got, want)
	}
}

// "/" finds a session by its space.
func TestFilterValueIncludesTheSpace(t *testing.T) {
	it := item{session: &serverv1.Session{Name: "s17", Space: "infra"}}
	if !strings.Contains(it.FilterValue(), "infra") {
		t.Errorf("FilterValue = %q, want the space in it", it.FilterValue())
	}
}

// A row from a listing of several hosts names its session with the host, so every action on it reaches that
// host; one from a single server's listing is the plain ID, as before.
func TestSessionRefQualifiesByHost(t *testing.T) {
	for _, tc := range []struct {
		s    *serverv1.Session
		ref  string
		name string
	}{
		{s: &serverv1.Session{Id: "abcd2345"}, ref: "@abcd2345", name: "work"},
		{s: &serverv1.Session{Id: "abcd2345", Host: "white"}, ref: "white:@abcd2345", name: "white:work"},
	} {
		if got := sessionRef(tc.s); got != tc.ref {
			t.Errorf("sessionRef(%v) = %q, want %q", tc.s, got, tc.ref)
		}
		if got := sessionName(tc.s, "work"); got != tc.name {
			t.Errorf("sessionName(%v, work) = %q, want %q", tc.s, got, tc.name)
		}
	}
}
