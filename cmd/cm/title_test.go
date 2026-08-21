package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

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

// titleCell extracts the rendered TITLE cell from a one-row table whose title is a run of x's.
//
// Found by measuring the longest run of x's and trailing dots rather than by splitting on whitespace,
// since the title is the only field made of those characters and tabwriter pads with spaces that carry no
// column boundary. Returned so a test can assert on its width, which is the thing the budget decides.
func titleCell(t *testing.T, table string) string {
	t.Helper()

	longest := ""
	for _, run := range strings.FieldsFunc(table, func(r rune) bool { return r != 'x' && r != '.' }) {
		if len(run) > len(longest) {
			longest = run
		}
	}
	if longest == "" {
		t.Fatalf("no title cell found in the table:\n%s", table)
	}
	return longest
}

// The width budget: TITLE gets whatever the other columns leave.
//
// Asserted as arithmetic here and as rendered output below, because the two can disagree. This is the
// part that decides whether a row fits, and the failure is not an error but a wrapped line.
func TestTitleWidth(t *testing.T) {
	for _, tc := range []struct {
		name     string
		termCols int
		reserved int
		want     int
	}{
		// The width left over, on a wide terminal.
		{name: "room to spare", termCols: 200, reserved: 60, want: 140},
		// Not a terminal: piped or redirected. A width that varied with the caller's window would make
		// piped output unreproducible, so it is the fixed width it always was.
		{name: "not a terminal", termCols: 0, reserved: 60, want: minTitleWidth},
		// Narrow terminals never go below the old fixed width. The row is already too wide there for a
		// reason TITLE cannot fix, since CWD is last and unbounded, so cutting the title shorter than
		// today would buy a table that still does not fit.
		{name: "no room left", termCols: 80, reserved: 70, want: minTitleWidth},
		// Reserved exceeding the terminal entirely, which a long path or a deep state cell reaches.
		{name: "reserved wider than the terminal", termCols: 80, reserved: 200, want: minTitleWidth},
		// Exactly at the floor, which must not round down to a title of zero width.
		{name: "budget equals the floor", termCols: 100, reserved: 100 - minTitleWidth, want: minTitleWidth},
		// One column past the floor is the first width that beats it.
		{name: "one past the floor", termCols: 101, reserved: 101 - minTitleWidth - 1, want: minTitleWidth + 1},
		// A nonsense negative can only come from a bug elsewhere, and must not produce a negative limit
		// that would panic the slice in truncate.
		{name: "negative width", termCols: -5, reserved: 10, want: minTitleWidth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleWidth(tc.termCols, tc.reserved); got != tc.want {
				t.Errorf("titleWidth(%d, %d) = %d, want %d", tc.termCols, tc.reserved, got, tc.want)
			}
		})
	}
}

// A wide terminal must actually show more of the title than the old fixed 30.
//
// The point of the change: a title is how a person recognizes which window a session is, and
// "claude: reviewing the wid..." identifies nothing.
func TestSessionsTableWidensTitleOnAWideTerminal(t *testing.T) {
	pinHome(t)

	s := sampleWireSession("work")
	s.Title = strings.Repeat("x", 120)
	s.Tags = nil

	var buf bytes.Buffer
	if err := printSessionsTableWidth(&buf, []*serverv1.Session{s}, 200); err != nil {
		t.Fatalf("printSessionsTableWidth() error = %v", err)
	}
	got := buf.String()

	// More than the fixed width it used to get, which is the whole point.
	if !strings.Contains(got, strings.Repeat("x", minTitleWidth+1)) {
		t.Errorf("table shows no more than the old fixed %d columns of title:\n%s", minTitleWidth, got)
	}
	// And still bounded, since 120 columns of title plus the other columns exceeds 200.
	if strings.Contains(got, s.Title) {
		t.Errorf("table shows the whole 120-column title on a 200-column terminal:\n%s", got)
	}
}

// The sized row has to fit the terminal it was sized for. This is what the arithmetic is for, and
// measuring the rendered line is the only way to know the budget agreed with the aligner.
//
// The invariant is conditional, and the condition is the design rather than a weakened assertion: TITLE is
// floored at minTitleWidth, so a terminal too narrow for the other columns alone wraps no matter what TITLE
// does. What must hold is that *whenever the dynamic budget was used*, the row fits. A row held at the
// floor is the case TITLE cannot fix, and TestSessionsTableKeepsTheFloorOnANarrowTerminal covers it.
//
// A title of 500 columns, so every width below truncates and the assertion is about the budget rather than
// about a title that happened to be short. Tags are cleared and the state cell left plain to keep the
// reserved cost realistic, since the fixture's "running(blocked: needs approval)" plus tags alone exceeds
// 80 columns and would floor every case.
func TestSessionsTableFitsTheTerminalWidth(t *testing.T) {
	pinHome(t)

	// From 100 up. At 80 this fixture's other columns leave less than the floor, so the title is held
	// there and the row wraps regardless: that is the floor's case, asserted by
	// TestSessionsTableKeepsTheFloorOnANarrowTerminal, and including it here would only assert that a
	// floored row wraps. The Fatalf below is what keeps this list honest, since a fixture that grows
	// cheaper or a floor that rises turns a case into a no-op and that must fail rather than pass.
	for _, termCols := range []int{100, 120, 160, 200, 400} {
		t.Run(fmt.Sprint(termCols), func(t *testing.T) {
			s := sampleWireSession("work")
			s.Title = strings.Repeat("x", 500)
			s.Tags = nil
			s.ReportedState = ""
			s.ReportedDetail = ""
			s.Busy = false
			s.Command = ""

			var buf bytes.Buffer
			if err := printSessionsTableWidth(&buf, []*serverv1.Session{s}, termCols); err != nil {
				t.Fatalf("printSessionsTableWidth() error = %v", err)
			}
			got := buf.String()

			// The rendered TITLE cell, measured rather than matched: the title is a run of x's ending in
			// the "..." marker, so the longest run of those characters in the row *is* the cell. An
			// earlier version compared against the floored string with strings.Contains, which silently
			// matched every width, since 27 x's plus "..." is a substring of any longer run: all six
			// cases skipped and the test proved nothing.
			width := len(titleCell(t, got))
			if width == minTitleWidth {
				t.Fatalf("title was held at the %d-column floor, so this width proves nothing about "+
					"the budget; pick a wider terminal or a cheaper fixture:\n%s", minTitleWidth, got)
			}

			// Runes rather than bytes throughout, since that is what a terminal lays out and what
			// tabwriter counted when it sized the columns.
			widest := 0
			for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				if n := len([]rune(line)); n > widest {
					widest = n
				}
			}

			// Both directions, because only one of them fails visibly. Too wide wraps, which is the bug
			// this arithmetic exists to prevent. Too narrow renders fine and silently hands TITLE less
			// width than the terminal had, so a one-sided check passes while the feature quietly does
			// nothing: over-reserving by the last column's padding was exactly that, and this is what
			// caught it.
			//
			// The title is 500 columns and every width here is well past the floor, so the row is
			// truncation-limited rather than content-limited and must land exactly on the terminal width.
			if widest != termCols {
				t.Errorf("widest line is %d columns on a %d-column terminal, want exactly %d:\n%s",
					widest, termCols, termCols, got)
			}
		})
	}
}

// A narrow terminal keeps the old fixed width rather than shrinking below it.
//
// Deliberate, and the reason is that TITLE cannot fix a narrow row: CWD sits last and is unbounded, so an
// 80-column terminal showing a deep path wraps whatever TITLE does. Shrinking would trade a shorter title
// for a table that still does not fit.
func TestSessionsTableKeepsTheFloorOnANarrowTerminal(t *testing.T) {
	pinHome(t)

	s := sampleWireSession("work")
	s.Title = strings.Repeat("x", 120)

	var buf bytes.Buffer
	if err := printSessionsTableWidth(&buf, []*serverv1.Session{s}, 40); err != nil {
		t.Fatalf("printSessionsTableWidth() error = %v", err)
	}
	// Exactly the floor: 27 x's plus the "..." marker.
	want := strings.Repeat("x", minTitleWidth-3) + "..."
	if !strings.Contains(buf.String(), want) {
		t.Errorf("table = %q, want the title held at the %d-column floor", buf.String(), minTitleWidth)
	}
}

// Piped output does not vary with the caller's terminal.
//
// A width that changed with whatever window ran the command would make `cm list > file` unreproducible and
// diff noisily between two people's terminals. A buffer is not a terminal, so this is also what every other
// table test in this package relies on to stay stable.
func TestSessionsTablePipedKeepsAFixedTitleWidth(t *testing.T) {
	pinHome(t)

	s := sampleWireSession("work")
	s.Title = strings.Repeat("x", 120)

	var buf bytes.Buffer
	// Through the exported entry point, so the not-a-terminal detection is what is under test rather
	// than a width passed in by hand.
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	want := strings.Repeat("x", minTitleWidth-3) + "..."
	if !strings.Contains(buf.String(), want) {
		t.Errorf("table = %q, want a fixed %d-column title when not writing to a terminal",
			buf.String(), minTitleWidth)
	}
}

// A title is cut at a rune boundary, not a byte offset.
//
// It used to be cut by byte: a title of accented characters came out as "ééé\xc3..." which a terminal paints
// as a replacement character, so the abbreviation marker sat next to visible corruption and read as cm
// mangling the title. Rune counting is also what tabwriter does when it sizes a column, so a byte count
// would disagree with the aligner and make the width budget wrong for any title that is not ASCII.
func TestSessionsTableTruncatesALongTitleAtRuneBoundaries(t *testing.T) {
	pinHome(t)

	s := sampleWireSession("work")
	// Two bytes per rune, so a byte-based cut lands mid-rune.
	s.Title = strings.Repeat("é", 120)

	var buf bytes.Buffer
	if err := printSessionsTableWidth(&buf, []*serverv1.Session{s}, 100); err != nil {
		t.Fatalf("printSessionsTableWidth() error = %v", err)
	}
	got := buf.String()
	if !utf8.ValidString(got) {
		t.Errorf("table is not valid UTF-8, so a rune was split:\n%q", got)
	}
	if strings.ContainsRune(got, utf8.RuneError) {
		t.Errorf("table contains a replacement character, so a rune was split:\n%q", got)
	}
}

// A multibyte value in a column TITLE is budgeted against must be measured in runes, not bytes.
//
// Separate from the test above, which covers cutting the title itself. This covers the other half: what
// the *other* columns are charged. tabwriter sizes a column by runes, so charging bytes over-reserves by
// one per extra byte and hands TITLE less width than the terminal had, which renders fine and is
// therefore invisible. A non-ASCII directory is the realistic source, since a path is the widest cell in
// the row and one under a name with accents is ordinary.
//
// Asserted as filling the terminal exactly rather than as merely fitting, since under-use is the whole
// failure mode here.
func TestSessionsTableBudgetsMultibyteColumnsByRune(t *testing.T) {
	pinHome(t)

	const termCols = 200

	s := sampleWireSession("work")
	s.Title = strings.Repeat("x", 500)
	s.Tags = nil
	s.ReportedState = ""
	s.ReportedDetail = ""
	s.Busy = false
	s.Command = ""
	// Two bytes per accented rune, so a byte count charges this cell 8 columns more than tabwriter will.
	s.Cwd = "/home/user/proyectos/años/mañana"
	s.CwdUri = "file://myhost" + s.Cwd

	var buf bytes.Buffer
	if err := printSessionsTableWidth(&buf, []*serverv1.Session{s}, termCols); err != nil {
		t.Fatalf("printSessionsTableWidth() error = %v", err)
	}
	got := buf.String()

	widest := 0
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if n := len([]rune(line)); n > widest {
			widest = n
		}
	}
	if widest != termCols {
		t.Errorf("widest line is %d columns on a %d-column terminal, want exactly %d; a multibyte cell "+
			"charged by bytes wastes title width:\n%s", widest, termCols, termCols, got)
	}
}
