package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// activeClientRow is one client marked as the active one, with every reported field set.
//
// Every field populated on purpose: the bug a fixture like this catches is a value rendered from the wrong
// source, which a zero value hides.
//
// The timestamps are literal strings rather than rendered from an instant, so a test asserting on the
// report does not have to care what zone the machine is in. clientRows is what converts an instant to a
// string, and TestClientRowsCarriesActive covers that conversion.
func activeClientRow() clientRowJSON {
	return clientRowJSON{
		Session:  "work",
		PID:      4242,
		Version:  "v0.4.1",
		Stale:    false,
		ReadOnly: false,
		// A fixed zone rather than the local one, because the rendering below is asserted verbatim: a
		// timestamp built in the local zone formats differently per machine, which is a failure that gets
		// blamed on the machine rather than on the test.
		AttachedAt:  new(pinnedZone()),
		Active:      true,
		LastInputAt: new(pinnedZone().Add(100 * time.Second)),
	}
}

// The detail report must carry every field, since a caller reads one value out of it by name.
//
// The whole rendering is asserted rather than a line at a time, because the failure this guards against is
// one field taking another's value: a check for "pid 4242" passes while the build column shows the pid.
func TestPrintClientDetail(t *testing.T) {
	var buf bytes.Buffer
	if err := printClientDetail(&buf, activeClientRow(), ""); err != nil {
		t.Fatalf("printClientDetail() error = %v", err)
	}
	want := strings.Join([]string{
		"session        work",
		"pid            4242",
		"kind           terminal",
		"build          v0.4.1",
		"stale          false",
		"attached_at    2023-11-14T14:13:20-08:00",
		"last_input_at  2023-11-14T14:15:00-08:00",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("printClientDetail() =\n%q\nwant\n%q", got, want)
	}
}

// --field must print one bare value, since that is what a keybinding or a script reads.
//
// Bare meaning no header, no padding, and nothing to strip: `cm clients current --field pid` is read
// directly into a variable, matching what `cm info --field cwd` does.
func TestPrintClientDetailField(t *testing.T) {
	for _, tc := range []struct{ field, want string }{
		{"session", "work\n"},
		{"pid", "4242\n"},
		{"kind", "terminal\n"},
		// The bare version, without the "(stale)" the table appends: a script comparing builds would have
		// to strip it, and staleness is its own field for that reason.
		{"build", "v0.4.1\n"},
		{"stale", "false\n"},
		{"attached_at", "2023-11-14T14:13:20-08:00\n"},
		{"last_input_at", "2023-11-14T14:15:00-08:00\n"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printClientDetail(&buf, activeClientRow(), tc.field); err != nil {
				t.Fatalf("printClientDetail(%q) error = %v", tc.field, err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("printClientDetail(%q) = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// An unknown field must name the ones that exist rather than printing nothing.
//
// A silent empty line is the bad outcome here: a script reading it would treat a typo as a legitimately
// empty value, which is the same failure mode as a missing session reporting no clients.
func TestPrintClientDetailUnknownField(t *testing.T) {
	var buf bytes.Buffer
	err := printClientDetail(&buf, activeClientRow(), "nope")
	if err == nil {
		t.Fatal("printClientDetail() accepted an unknown field")
	}
	if !strings.Contains(err.Error(), "last_input_at") {
		t.Errorf("error does not list the available fields: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("printClientDetail() wrote %q for an unknown field, want nothing", buf.String())
	}
}

// A follower renders as one, and a client that never typed says so rather than showing a blank.
//
// Unreachable through `cm clients current`, which refuses when there is no active client, and asserted
// because this renders any row: an empty value in a report of named fields reads as a formatting bug
// rather than as "never".
func TestPrintClientDetailFollowerAndNeverTyped(t *testing.T) {
	var buf bytes.Buffer
	row := clientRowJSON{Session: "work", PID: 100, ReadOnly: true, Stale: true}
	if err := printClientDetail(&buf, row, ""); err != nil {
		t.Fatalf("printClientDetail() error = %v", err)
	}
	want := strings.Join([]string{
		"session        work",
		"pid            100",
		"kind           follower",
		// No version reported, which is what a client predating the field sends.
		"build          unknown",
		"stale          true",
		"attached_at    unknown",
		"last_input_at  never",
		"",
	}, "\n")
	if got := buf.String(); got != want {
		t.Errorf("printClientDetail() =\n%q\nwant\n%q", got, want)
	}
}

// The field list backing --field must stay in step with what the report prints.
func TestClientFieldNames(t *testing.T) {
	want := []string{
		"session", "pid", "kind", "build", "stale", "attached_at", "last_input_at",
	}
	if got := ClientFieldNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("ClientFieldNames() = %v\nwant %v", got, want)
	}
}

// The JSON shape is a contract, so the exact key set is asserted rather than a few fields.
//
// Adding a key is fine; renaming or dropping one breaks callers. Same rule as TestSessionJSONKeys, and it
// applies to this row type because `cm clients list --json` and `cm clients current --json` both emit it.
func TestClientRowJSONKeys(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, activeClientRow()); err != nil {
		t.Fatalf("writeJSON() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	want := []string{
		"session", "pid", "version", "stale", "read_only",
		"attached_at",
		// Which client is being used, and when it last typed.
		"active", "last_input_at",
		// What the client can do, which is the question its build string leaves open.
		"capabilities",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("output is missing key %q:\n%s", k, buf.String())
		}
	}
	if len(got) != len(want) {
		t.Errorf("output has %d keys, want %d; an unexpected key is a contract change:\n%s",
			len(got), len(want), buf.String())
	}
}

// The active mark and the input time have to survive the wire, not just exist on the server.
//
// clientRows is where a new field is most likely to be forgotten, since it copies the wire message field
// by field: the server can compute the mark perfectly and the CLI still show nothing.
func TestClientRowsCarriesActive(t *testing.T) {
	sessions := []*serverv1.Session{{
		Name: "work",
		AttachedClients: []*serverv1.AttachedClient{
			{Pid: 100, Version: "current", AttachedAtUnix: 1_700_000_000},
			{
				Pid: 101, Version: "current", AttachedAtUnix: 1_700_000_050,
				Active: true, LastInputAtUnix: 1_700_000_100,
			},
		},
	}}
	got := clientRows(sessions, nil, "current", false)
	want := []clientRowJSON{
		{
			Session: "work", PID: 100, Version: "current", Capabilities: []string{},
			AttachedAt: at(1_700_000_000),
			// Never typed, so no time and no mark. Nil rather than an instant of zero, which would render
			// as a client that typed decades ago.
			LastInputAt: nil, Active: false,
		},
		{
			Session: "work", PID: 101, Version: "current", Capabilities: []string{},
			AttachedAt: at(1_700_000_050), LastInputAt: at(1_700_000_100), Active: true,
		},
	}
	// Pinned rather than copied out of the result. The string form could not be: it rendered in the
	// machine's zone, so an expected literal passed where it was written and failed in CI, and the only
	// portable want was "whatever we just produced" -- which a blank value also satisfies. An instant is
	// the same number everywhere.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clientRows() = %+v\nwant %+v", got, want)
	}
	// Stated again on its own, because it is the case the compare above would have hidden while the
	// timestamps were copied from the result, and it is the one a reader gets wrong.
	if got[0].LastInputAt != nil {
		t.Errorf("LastInputAt = %v for a client that never typed, want nil", got[0].LastInputAt)
	}
}

// The table must mark the active client and explain the mark.
func TestPrintClientRowsMarksActive(t *testing.T) {
	var buf bytes.Buffer
	rows := clientRows([]*serverv1.Session{{
		Name: "work",
		AttachedClients: []*serverv1.AttachedClient{
			{Pid: 100, Version: "current", AttachedAtUnix: 1_700_000_000},
			{
				Pid: 101, Version: "current", AttachedAtUnix: 1_700_000_050,
				Active: true, LastInputAtUnix: 1_700_000_100,
			},
		},
	}}, nil, "current", false)
	if err := printClientRows(&buf, rows, "current"); err != nil {
		t.Fatalf("printClientRows() error = %v", err)
	}
	got := buf.String()

	// The mark is on the active client's line and nowhere else, which a bare Contains("*") would not show.
	for _, line := range strings.Split(got, "\n") {
		switch {
		case strings.Contains(line, "101"):
			if !strings.HasPrefix(line, "*") {
				t.Errorf("the active client's row is not marked:\n%s", got)
			}
		case strings.Contains(line, "100"):
			if strings.HasPrefix(line, "*") {
				t.Errorf("an inactive client's row is marked:\n%s", got)
			}
		}
	}
	if !strings.Contains(got, "* the client last typed in") {
		t.Errorf("table does not explain the mark:\n%s", got)
	}
}

// With nothing marked, the legend must not appear.
//
// Explaining a symbol that is nowhere in the output invites a hunt for it, and this is the ordinary state
// of a session nobody has typed in yet.
func TestPrintClientRowsNoLegendWithoutActive(t *testing.T) {
	var buf bytes.Buffer
	rows := clientRows([]*serverv1.Session{{
		Name:            "work",
		AttachedClients: []*serverv1.AttachedClient{{Pid: 100, Version: "current"}},
	}}, nil, "current", false)
	if err := printClientRows(&buf, rows, "current"); err != nil {
		t.Fatalf("printClientRows() error = %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "*") {
		t.Errorf("table mentions the mark with nothing marked:\n%s", got)
	}
	if strings.Contains(got, "last typed") {
		t.Errorf("table explains the mark with nothing marked:\n%s", got)
	}
}
