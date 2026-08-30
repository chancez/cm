package main

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// sampleClientSessions returns two sessions whose clients span builds, including one that reported none.
func sampleClientSessions() []*serverv1.Session {
	return []*serverv1.Session{
		{
			Name: "work",
			AttachedClients: []*serverv1.AttachedClient{
				{Pid: 100, Version: "current", AttachedAtUnix: 1_700_000_000},
				{Pid: 101, Version: "old", ReadOnly: true, AttachedAtUnix: 1_700_000_100},
			},
		},
		{
			Name: "other",
			AttachedClients: []*serverv1.AttachedClient{
				// No version and no timestamp, which is what a client predating those fields sends.
				{Pid: 102},
			},
		},
	}
}

// Flattening must produce one row per client, with staleness resolved against the server's build.
//
// The whole value is asserted rather than a field at a time, because the bug this guards against is a field
// being wrong while the others are right: staleness is computed here, so a check that only looked at pids
// would pass with every client marked current.
func TestClientRows(t *testing.T) {
	got := clientRows(sampleClientSessions(), nil, "current", false)
	want := []clientRowJSON{
		{
			Session: "work", PID: 100, Version: "current", Stale: false,
			AttachedAt: at(1_700_000_000),
		},
		{
			Session: "work", PID: 101, Version: "old", Stale: true, ReadOnly: true,
			AttachedAt: at(1_700_000_100),
		},
		{
			// A client that reported no build counts as stale: the field exists because older clients did
			// not send one, so unknown is more likely behind than current, and calling it current would
			// hide exactly what --stale is for.
			Session: "other", PID: 102, Version: "", Stale: true,
			// No timestamp, so AttachedAt stays nil rather than rendering an instant.
			AttachedAt: nil,
		},
	}
	// The instants are pinned rather than copied out of the result, which the string form could not do:
	// it rendered in the machine's zone, so the only portable want was the value just produced, and a
	// blank satisfied that too.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clientRows() = %+v\nwant %+v", got, want)
	}
	if got[2].AttachedAt != nil {
		t.Errorf("AttachedAt = %v for a client that reported no time, want nil", got[2].AttachedAt)
	}
}

// --stale must keep only the clients that are not on the server's build.
func TestClientRowsStaleOnly(t *testing.T) {
	got := clientRows(sampleClientSessions(), nil, "current", true)
	if len(got) != 2 {
		t.Fatalf("clientRows(stale) returned %d rows, want 2:\n%+v", len(got), got)
	}
	for _, r := range got {
		if !r.Stale {
			t.Errorf("--stale returned a current client: %+v", r)
		}
	}
	// The pids, so this cannot pass by returning the wrong two rows.
	if got[0].PID != 101 || got[1].PID != 102 {
		t.Errorf("clientRows(stale) pids = %d, %d; want 101, 102", got[0].PID, got[1].PID)
	}
}

// Naming sessions must restrict the listing to them.
func TestClientRowsFiltersBySession(t *testing.T) {
	got := clientRows(sampleClientSessions(), []string{"other"}, "current", false)
	want := []clientRowJSON{{Session: "other", PID: 102, Stale: true}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clientRows(names) = %+v\nwant %+v", got, want)
	}
}

// A server whose version could not be read must not mark every client stale.
//
// Status can fail against an older server that does not report one, and flagging every client in that case
// would be a table full of warnings about nothing. An empty server version compares equal to an empty
// client version, so the unknown-versus-unknown case is quiet; a client that does report one is still
// flagged, which is honest, since the comparison genuinely cannot be made.
func TestClientRowsWithUnknownServerVersion(t *testing.T) {
	got := clientRows([]*serverv1.Session{{
		Name:            "work",
		AttachedClients: []*serverv1.AttachedClient{{Pid: 100}},
	}}, nil, "", false)
	want := []clientRowJSON{{Session: "work", PID: 100, Stale: false}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("clientRows() with no server version = %+v\nwant %+v", got, want)
	}
}

// An empty listing must say so rather than printing a bare header.
//
// Ambiguous in a way an empty `cm list` is not: it can mean no sessions or sessions with nothing attached,
// and under --stale it is the good outcome, so a header with no rows reads as a broken command.
func TestPrintClientRowsEmpty(t *testing.T) {
	var buf bytes.Buffer
	if err := printClientRows(&buf, nil, "current"); err != nil {
		t.Fatalf("printClientRows() error = %v", err)
	}
	if got := buf.String(); got != "no clients attached\n" {
		t.Errorf("printClientRows() with no rows = %q, want a plain statement", got)
	}
}

// The table must name the server's build and point at the remedy when anything is stale.
//
// A bare commit hash per row says nothing without something to compare against, and the reason to surface
// staleness at all is that it is fixable without losing work, which is not obvious.
func TestPrintClientRowsNamesTheRemedy(t *testing.T) {
	var buf bytes.Buffer
	rows := clientRows(sampleClientSessions(), nil, "current", false)
	if err := printClientRows(&buf, rows, "current"); err != nil {
		t.Fatalf("printClientRows() error = %v", err)
	}
	got := buf.String()
	for _, want := range []string{
		"server is current",
		"2 client(s) on another build",
		"cm clients upgrade --all",
		// A client that reported nothing is shown as unknown rather than as an empty column.
		"unknown (stale)",
		// Followers are distinguished, since one never sizes a session or answers a query.
		"follower",
		"terminal",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("table is missing %q:\n%s", want, got)
		}
	}
}

// With nothing stale, the table must not suggest an upgrade.
func TestPrintClientRowsQuietWhenCurrent(t *testing.T) {
	var buf bytes.Buffer
	rows := clientRows([]*serverv1.Session{{
		Name:            "work",
		AttachedClients: []*serverv1.AttachedClient{{Pid: 100, Version: "current"}},
	}}, nil, "current", false)
	if err := printClientRows(&buf, rows, "current"); err != nil {
		t.Fatalf("printClientRows() error = %v", err)
	}
	got := buf.String()
	if strings.Contains(got, "upgrade") {
		t.Errorf("table suggests an upgrade with nothing stale:\n%s", got)
	}
	if strings.Contains(got, "stale") {
		t.Errorf("table mentions staleness with nothing stale:\n%s", got)
	}
}
