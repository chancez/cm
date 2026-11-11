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
