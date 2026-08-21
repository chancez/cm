package main

import (
	"bytes"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// The title is its own column rather than folded into STATE.
//
// They answer different questions: the title says what a window is, the state says whether it is safe
// to close. Conflating them is what made the state column carry running(nvim) already, and a title is
// the thing a person actually recognizes a window by.
func TestSessionsTableShowsTitleAsItsOwnColumn(t *testing.T) {
	s := sampleWireSession("work")
	s.Title = "~/p/cm (main)"
	// A reported state too, so a title cannot be mistaken for having replaced the state column.
	s.ReportedState = "blocked"
	s.ReportedDetail = "needs approval"

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()

	for _, want := range []string{"TITLE", "~/p/cm (main)", "STATE", "blocked"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}

	// TITLE sits before CWD, since a path is the one unbounded field and nothing after it aligns.
	header := strings.SplitN(got, "\n", 2)[0]
	if strings.Index(header, "TITLE") > strings.Index(header, "CWD") {
		t.Errorf("header = %q, want TITLE before CWD", header)
	}
}

// No title anywhere means no column, so a shell that reports none costs no width.
//
// The same reasoning as the tags column: CWD is a full path and sits last, so every column before it
// eats into the path.
func TestSessionsTableOmitsTitleWhenNoneReported(t *testing.T) {
	s := sampleWireSession("work")
	s.Title = ""

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	if strings.Contains(buf.String(), "TITLE") {
		t.Errorf("table has a TITLE column with no titles reported:\n%s", buf.String())
	}
}

// One titled session adds the column for every row, or the header and rows disagree.
//
// A per-row decision misaligns the table, which is worse than an empty cell: the untitled row would
// put its CWD under the header's TITLE.
func TestSessionsTableTitleColumnIsAllOrNothing(t *testing.T) {
	// The offsets below are found by searching for the fixture's directory, so it must not abbreviate.
	pinHome(t)

	titled := sampleWireSession("titled")
	titled.Title = "~/p/cm (main)"
	titled.Tags = nil
	untitled := sampleWireSession("plain")
	untitled.Title = ""
	untitled.Tags = nil

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{titled, untitled}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header and two rows:\n%s", len(lines), buf.String())
	}
	// Measured by offset rather than field count, since a title contains spaces and splitting on
	// whitespace would not find column boundaries.
	want := strings.Index(lines[0], "CWD")
	if want < 0 {
		t.Fatalf("header has no CWD column:\n%s", buf.String())
	}
	for _, line := range lines[1:] {
		if got := strings.Index(line, "/home/user/projects"); got != want {
			t.Errorf("row %q starts CWD at %d, header has it at %d:\n%s",
				line, got, want, buf.String())
		}
	}
}

// Title and tags together must both appear, and in a fixed order.
//
// Two optional columns is where a format-string-per-combination approach drifts: four combinations,
// each with its own column order to keep in step.
func TestSessionsTableWithTitleAndTags(t *testing.T) {
	// The values checked below include the fixture's directory, so it must not abbreviate.
	pinHome(t)

	s := sampleWireSession("work")
	s.Title = "~/p/cm (main)"
	s.Tags = map[string]string{"project": "cm"}

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()
	header := strings.SplitN(got, "\n", 2)[0]

	iTitle, iTags, iCwd := strings.Index(header, "TITLE"), strings.Index(header, "TAGS"), strings.Index(header, "CWD")
	if iTitle < 0 || iTags < 0 || iCwd < 0 {
		t.Fatalf("header = %q, want TITLE, TAGS, and CWD", header)
	}
	if !(iTitle < iTags && iTags < iCwd) {
		t.Errorf("header = %q, want the order TITLE, TAGS, CWD", header)
	}
	// And the values line up under them rather than only the headers being present.
	for _, want := range []string{"~/p/cm (main)", "project=cm", "/home/user/projects"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
}

// A long title is truncated rather than left to wreck the column.
//
// Nothing bounds what a program emits as a title, and the full value is in `cm info` and the JSON.
func TestSessionsTableTruncatesALongTitle(t *testing.T) {
	s := sampleWireSession("work")
	s.Title = strings.Repeat("verylongtitle", 6)

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "...") {
		t.Errorf("table = %q, want a long title truncated", got)
	}
	if strings.Contains(got, s.Title) {
		t.Errorf("table contains the whole long title, want it cut:\n%s", got)
	}
}
