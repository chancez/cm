package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A leading session name has to be told apart from a bare tag key, since both are bare words.
func TestSplitTagArgs(t *testing.T) {
	// Set so the "no session given" path resolves rather than erroring, which is what a program
	// tagging itself relies on.
	t.Setenv(paths.SessionEnv(), "current")

	tests := []struct {
		name        string
		args        []string
		wantSession string
		wantTags    []string
	}{
		{
			// The '=' decides it: the first argument is a tag, so the session is the calling one.
			name:        "all tags uses the calling session",
			args:        []string{"project=cm", "role=reviewer"},
			wantSession: "current",
			wantTags:    []string{"project=cm", "role=reviewer"},
		},
		{
			name:        "leading name",
			args:        []string{"s7", "project=cm"},
			wantSession: "s7",
			wantTags:    []string{"project=cm"},
		},
		{
			// A bare word with no '=' is read as a session name, which is the reading that fails
			// loudly: naming a session and giving it no tags is refused by the caller, while tagging
			// the wrong session with the right key would go unreported.
			name:        "a bare word is a session name",
			args:        []string{"build"},
			wantSession: "build",
			wantTags:    nil,
		},
		{
			name:        "no arguments at all",
			args:        nil,
			wantSession: "current",
			wantTags:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			session, tagArgs, err := splitTagArgs(tc.args)
			if err != nil {
				t.Fatalf("splitTagArgs(%v) error = %v", tc.args, err)
			}
			if session != tc.wantSession {
				t.Errorf("splitTagArgs(%v) session = %q, want %q", tc.args, session, tc.wantSession)
			}
			if len(tagArgs) == 0 && len(tc.wantTags) == 0 {
				return
			}
			if !reflect.DeepEqual(tagArgs, tc.wantTags) {
				t.Errorf("splitTagArgs(%v) tags = %v, want %v", tc.args, tagArgs, tc.wantTags)
			}
		})
	}
}

// With no session named and no CM_SESSION, there is nothing to tag, and the error has to say so.
func TestSplitTagArgsWithoutASession(t *testing.T) {
	t.Setenv(paths.SessionEnv(), "")

	_, _, err := splitTagArgs([]string{"project=cm"})
	if err == nil {
		t.Fatal("splitTagArgs() = nil error, want one when no session can be resolved")
	}
	// Names the variable, since setting it is the fix and a bare "no session" would not say that.
	if !strings.Contains(err.Error(), paths.SessionEnv()) {
		t.Errorf("error = %q, want it to name %s", err, paths.SessionEnv())
	}
}

// The tags column appears only when something is tagged.
//
// Not cosmetic: CWD is a full path and sits last, so an unconditional column pushes it off the edge of
// a normal terminal for everyone who does not use tags.
func TestSessionsTableOmitsTagsWhenNothingIsTagged(t *testing.T) {
	s := sampleWireSession("work")
	s.Tags = nil

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	if strings.Contains(buf.String(), "TAGS") {
		t.Errorf("table has a TAGS column with nothing tagged:\n%s", buf.String())
	}
}

func TestSessionsTableShowsTags(t *testing.T) {
	s := sampleWireSession("work")
	s.Tags = map[string]string{"project": "cm", "review": ""}

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "TAGS") {
		t.Errorf("table has no TAGS column for a tagged session:\n%s", got)
	}
	// Sorted and rendered the way Format does, with a valueless key printed alone.
	if !strings.Contains(got, "project=cm,review") {
		t.Errorf("table = %q, want it to contain %q", got, "project=cm,review")
	}
	// The column sits before CWD, since a path can be arbitrarily long and nothing after it aligns.
	header := strings.SplitN(got, "\n", 2)[0]
	if strings.Index(header, "TAGS") > strings.Index(header, "CWD") {
		t.Errorf("header = %q, want TAGS before CWD", header)
	}
}

// One tagged session must add the column for every row, or the header and the rows disagree and the
// columns stop lining up.
func TestSessionsTableTagsColumnIsAllOrNothing(t *testing.T) {
	tagged := sampleWireSession("tagged")
	tagged.Tags = map[string]string{"project": "cm"}
	untagged := sampleWireSession("plain")
	untagged.Tags = nil

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{tagged, untagged}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}

	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want a header and two rows:\n%s", len(lines), buf.String())
	}

	// Alignment is the thing under test, so it is measured rather than inferred from a field count:
	// the state column contains spaces, so splitting on whitespace does not find column boundaries.
	// An untagged row that skipped the tab would put its CWD under the header's TAGS.
	//
	// CWD is the column after TAGS, so if every row starts it at the same offset as the header, the
	// untagged row emitted its empty tags cell.
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

// A long tag set is truncated rather than left to wreck the column.
//
// Each tag is bounded at 63 bytes, but nothing bounds how many a session carries, so the rendered set
// is unbounded even though its parts are not.
func TestSessionsTableTruncatesLongTags(t *testing.T) {
	s := sampleWireSession("work")
	s.Tags = map[string]string{
		"aaaaaaaaaaaaaaa": "1111111111111111",
		"bbbbbbbbbbbbbbb": "2222222222222222",
		"ccccccccccccccc": "3333333333333333",
	}

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "...") {
		t.Errorf("table = %q, want a long tag set truncated", got)
	}
	// The full set is in `cm info` and the JSON output, so nothing is lost.
	if strings.Contains(got, "3333333333333333") {
		t.Errorf("table = %q, want the tail of a long tag set cut", got)
	}
}

// `cm info --field tags` prints the rendered set, since a field prints one bare value.
func TestPrintSessionInfoTagsField(t *testing.T) {
	s := sampleWireSession("work")
	s.Tags = map[string]string{"project": "cm", "review": ""}

	var buf bytes.Buffer
	if err := printSessionInfo(&buf, s, "tags"); err != nil {
		t.Fatalf("printSessionInfo() error = %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "project=cm,review" {
		t.Errorf("info --field tags = %q, want %q", got, "project=cm,review")
	}
}
