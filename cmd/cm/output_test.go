package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func sampleWireSession(name string) *serverv1.Session {
	return &serverv1.Session{
		Name:          name,
		ShellPid:      4242,
		Clients:       2,
		Cwd:           "/home/user/projects",
		CwdUri:        "file://myhost/home/user/projects",
		CwdIsLocal:    true,
		Title:         "editing",
		CreatedAtUnix: 1_700_000_000,
		State:         serverv1.SessionState_SESSION_STATE_RUNNING,
		Busy:          true,
		Command:       "nvim notes.md",
		// A reported state, since it is part of the contract and the table renders it in preference to
		// the derived one.
		ReportedState:  "blocked",
		ReportedDetail: "needs approval",
		ReportedSource: "my-agent",
		Tags:           map[string]string{"project": "cm", "review": ""},
		// Left empty, so this fixture describes the ordinary session. Nesting is exercised by the
		// tests that assert on it directly, where the annotation is the thing under test.
	}
}

// at builds an optional timestamp for a fixture from a unix instant.
//
// Lets a want pin the instant rather than copying it out of the result, which is what the string form
// forced: it rendered in the local zone, so the only portable assertion was "whatever we just produced".
// Both sides come from time.Unix here, so they carry the same *time.Location, which is what
// reflect.DeepEqual compares inside a time.Time.
func at(unix int64) *time.Time {
	t := time.Unix(unix, 0)
	return &t
}

// pinnedZone is a fixture instant in a fixed zone, for tests that assert on rendered output.
//
// A fixed offset rather than time.Unix, because the render preserves the zone: a fixture built from the
// local zone would produce a different string on a machine in another one, which is the same class of
// machine-dependent failure pinHome exists to prevent.
func pinnedZone() time.Time {
	return time.Date(2023, 11, 14, 14, 13, 20, 0, time.FixedZone("", -8*60*60))
}

// pinHome points HOME somewhere unrelated to the fixture's directory, so the CWD column renders that
// directory verbatim.
//
// Needed because the column abbreviates a path under home to "~/...", and the fixture's
// /home/user/projects is under the home directory of a real developer whose account is "user" on Linux.
// Without this, a test looking for the literal path passes on one machine and fails on another, which is
// the kind of failure that gets blamed on the machine rather than on the test.
func pinHome(t *testing.T) {
	t.Helper()
	// os.UserHomeDir reads HOME on both platforms cm supports.
	t.Setenv("HOME", "/pinned-elsewhere")
}

// The JSON shape is a contract that scripts depend on, so the exact key set is asserted rather than
// a few fields. Adding a key is fine; renaming or dropping one breaks callers.
func TestSessionJSONKeys(t *testing.T) {
	var buf bytes.Buffer
	if err := writeJSON(&buf, toSessionJSON(sampleWireSession("work"))); err != nil {
		t.Fatalf("writeJSON() error = %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}

	want := []string{
		// name is what to show a person; id is the identity, and names is every name bound to the
		// session, since a session can have several or none.
		"name", "id", "names",
		"state", "shell_pid", "clients", "exit_code", "title",
		"cwd", "cwd_uri", "cwd_is_local", "created_at",
		"busy", "command",
		// The last command's own outcome, distinct from exit_code above, which is the session's.
		"last_command_exit_code", "command_finished",
		// reported_at makes the state readable: a report stands until the program changes it and
		// survives a server restart, so its age is what separates "blocked now" from "blocked at 9am".
		"reported_state", "reported_detail", "reported_source", "reported_at",
		"tags", "hosting",
		// What is attached, alongside the "clients" count above.
		"attached_clients",
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

func TestSessionJSONValues(t *testing.T) {
	got := toSessionJSON(sampleWireSession("work"))
	want := sessionJSON{
		Name:           "work",
		State:          "running",
		ShellPID:       4242,
		Clients:        2,
		ExitCode:       0,
		Title:          "editing",
		Cwd:            "/home/user/projects",
		CwdURI:         "file://myhost/home/user/projects",
		CwdIsLocal:     true,
		CreatedAt:      time.Unix(1_700_000_000, 0),
		Busy:           true,
		Command:        "nvim notes.md",
		ReportedState:  "blocked",
		ReportedDetail: "needs approval",
		ReportedSource: "my-agent",
		Tags:           map[string]string{"project": "cm", "review": ""},
		// Empty rather than nil, like Tags, so a script indexing into it needs no null check.
		Hosting: []string{},
		// Also empty rather than nil. The fixture describes a session whose clients the server did not
		// report, which is what an older server looks like; TestSessionJSONReportsAttachedClients
		// covers the populated case.
		AttachedClients: []attachedClientJSON{},
	}
	// DeepEqual rather than ==, since the struct holds a map and pointers. Still the whole value, which
	// is the point: a field-by-field check passes while the rest of the struct is wrong.
	//
	// The timestamp is pinned rather than copied from the result, which the string form could not do: it
	// rendered in the local zone, so an expected literal only held on one machine. A time.Time carries the
	// instant and the zone separately, so the fixture states the instant and the zone stops mattering.
	if !reflect.DeepEqual(got, want) {
		t.Errorf("toSessionJSON() = %+v\nwant %+v", got, want)
	}
}

// Each attached client must be reported with its build, since that is what the count cannot say.
//
// The count alone is what made a real investigation slow: after losing a session, working out what was
// attached meant reading `ps` and matching binary inodes with lsof to find which clients ran which
// build. A shim outlives servers by design, so several builds at once is normal rather than a fault,
// and one incident had twelve across twenty-six sessions.
//
// Two clients, deliberately, with the second read-only and reporting no version. That is what an older
// client looks like on the wire, and reporting an unknown build as empty rather than inventing one is
// the behavior worth pinning.
func TestSessionJSONReportsAttachedClients(t *testing.T) {
	s := sampleWireSession("work")
	s.AttachedClients = []*serverv1.AttachedClient{
		{Pid: 4242, Version: "v0.1.2-9-g4352aa4", AttachedAtUnix: 1_700_000_000},
		{Pid: 5150, ReadOnly: true},
	}

	got := toSessionJSON(s).AttachedClients
	want := []attachedClientJSON{
		{
			PID:        4242,
			Version:    "v0.1.2-9-g4352aa4",
			AttachedAt: at(1_700_000_000),
			// Empty rather than nil, and spelled out because the two are indistinguishable in a failure
			// message: %+v prints both as [], so a nil-against-empty mismatch here reports a got and a want
			// that look identical and gives the reader nothing to go on.
			Capabilities: []string{},
		},
		{
			PID:      5150,
			ReadOnly: true,
			// No version and no timestamp. AttachedAt stays nil rather than rendering an instant, which
			// would read as a client attached decades ago instead of one whose time is unknown.
			//
			// Capabilities is empty for the same reason it is above, and means something different from the
			// version being empty: a client that reports none predates the mechanism, so nothing about what
			// it can do is established. See capability.Support.
			Capabilities: []string{},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachedClients = %+v\nwant %+v", got, want)
	}
	// Stated separately as well as inside the whole-value compare, because it is the case a reader gets
	// wrong: null means the client reported no time, and is not the same as an instant of zero.
	if got[1].AttachedAt != nil {
		t.Errorf("AttachedAt = %v for a client that reported no time, want nil", got[1].AttachedAt)
	}
}

// A remote directory must not be handed over as a local path, since acting on it would open the
// wrong place or fail. The URI is still reported, so a caller can tell "remote" from "unknown".
func TestSessionJSONWithholdsRemoteCwd(t *testing.T) {
	s := sampleWireSession("remote")
	s.CwdIsLocal = false
	s.Cwd = "/remote/path"
	s.CwdUri = "file://otherhost/remote/path"

	got := toSessionJSON(s)
	if got.Cwd != "" {
		t.Errorf("Cwd = %q, want empty for a remote directory", got.Cwd)
	}
	if got.CwdURI != "file://otherhost/remote/path" {
		t.Errorf("CwdURI = %q, want the reported URI so a caller can see it is remote", got.CwdURI)
	}
	if got.CwdIsLocal {
		t.Error("CwdIsLocal = true, want false")
	}
}

// The state enum distinguishes a shell that exited from a shim that vanished, which the older
// boolean could not.
func TestStateName(t *testing.T) {
	tests := []struct {
		name  string
		state serverv1.SessionState
		// exited is the legacy field, set to prove the enum wins when both are present.
		exited bool
		want   string
	}{
		{"running", serverv1.SessionState_SESSION_STATE_RUNNING, false, "running"},
		{"exited", serverv1.SessionState_SESSION_STATE_EXITED, true, "exited"},
		{"dead", serverv1.SessionState_SESSION_STATE_DEAD, true, "dead"},
		// A server predating the enum sends only the boolean, and a newer client must still report
		// something truthful rather than "unspecified".
		{"legacy running", serverv1.SessionState_SESSION_STATE_UNSPECIFIED, false, "running"},
		{"legacy exited", serverv1.SessionState_SESSION_STATE_UNSPECIFIED, true, "exited"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &serverv1.Session{State: tt.state, Exited: tt.exited}
			if got := stateName(s); got != tt.want {
				t.Errorf("stateName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// An empty list must be an array, not null, so a script can iterate without special-casing.
func TestPrintSessionsJSONEmptyIsArray(t *testing.T) {
	var buf bytes.Buffer
	if err := printSessionsJSON(&buf, nil); err != nil {
		t.Fatalf("printSessionsJSON() error = %v", err)
	}
	got := strings.TrimSpace(buf.String())
	if got != "[]" {
		t.Errorf("output = %q, want %q", got, "[]")
	}

	var arr []sessionJSON
	if err := json.Unmarshal(buf.Bytes(), &arr); err != nil {
		t.Errorf("empty output does not unmarshal as an array: %v", err)
	}
}

// Ordering must be stable across calls, or a script diffing output sees spurious changes.
func TestPrintSessionsJSONIsOrdered(t *testing.T) {
	mk := func(name string, created int64) *serverv1.Session {
		s := sampleWireSession(name)
		s.CreatedAtUnix = created
		return s
	}
	// Same creation time for two of them, so the name tiebreak is exercised.
	sessions := []*serverv1.Session{
		mk("zeta", 200),
		mk("beta", 100),
		mk("alpha", 100),
	}

	var buf bytes.Buffer
	if err := printSessionsJSON(&buf, sessions); err != nil {
		t.Fatalf("printSessionsJSON() error = %v", err)
	}
	var got []sessionJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	want := []string{"alpha", "beta", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("got %d sessions, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Name != want[i] {
			var names []string
			for _, s := range got {
				names = append(names, s.Name)
			}
			t.Errorf("order = %v, want %v", names, want)
			break
		}
	}
}

func TestPrintSessionInfoField(t *testing.T) {
	s := sampleWireSession("work")

	tests := []struct {
		field string
		want  string
	}{
		{"name", "work"},
		{"state", "running"},
		{"pid", "4242"},
		{"clients", "2"},
		{"title", "editing"},
		{"cwd", "/home/user/projects"},
		{"cwd_uri", "file://myhost/home/user/projects"},
		{"cwd_is_local", "true"},
	}
	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printSessionInfo(&buf, s, tt.field); err != nil {
				t.Fatalf("printSessionInfo() error = %v", err)
			}
			// Bare value with a newline and nothing else, since a script consumes this directly.
			if got := buf.String(); got != tt.want+"\n" {
				t.Errorf("output = %q, want %q", got, tt.want+"\n")
			}
		})
	}
}

func TestPrintSessionInfoRejectsUnknownField(t *testing.T) {
	var buf bytes.Buffer
	err := printSessionInfo(&buf, sampleWireSession("work"), "nonsense")
	if err == nil {
		t.Fatal("printSessionInfo() = nil error for an unknown field, want a rejection")
	}
	// The message must list the valid fields, since guessing is the alternative.
	if !strings.Contains(err.Error(), "cwd_uri") {
		t.Errorf("error %q does not list the available fields", err)
	}
}

// A partial failure must be reported in full and still set a non-zero exit status, since killing
// several sessions can partly succeed.
func TestReportKillPartialFailure(t *testing.T) {
	resp := &serverv1.KillResponse{
		Killed: []string{"ok-one", "ok-two"},
		Errors: map[string]string{"bad": "shim unreachable"},
	}

	var text bytes.Buffer
	err := reportKill(&text, resp, false)
	if err == nil {
		t.Error("reportKill() = nil error with a failed session, want an error for the exit status")
	}
	for _, want := range []string{"killed ok-one", "killed ok-two"} {
		if !strings.Contains(text.String(), want) {
			t.Errorf("output %q missing %q", text.String(), want)
		}
	}
	if err != nil && !strings.Contains(err.Error(), "shim unreachable") {
		t.Errorf("error %q does not say why", err)
	}

	var asJSON bytes.Buffer
	err = reportKill(&asJSON, resp, true)
	// In JSON mode the detail is in the payload, so the error only carries the exit status.
	if !errors.Is(err, errAlreadyReported) {
		t.Errorf("JSON mode error = %v, want errAlreadyReported so the payload is not duplicated", err)
	}
	var got killJSON
	if err := json.Unmarshal(asJSON.Bytes(), &got); err != nil {
		t.Fatalf("JSON output does not unmarshal: %v\n%s", err, asJSON.String())
	}
	if len(got.Killed) != 2 || got.Errors["bad"] != "shim unreachable" {
		t.Errorf("payload = %+v, want both outcomes reported", got)
	}
}

// Null slices and maps would force a script to special-case them, so both are emitted as empty
// containers.
func TestReportKillEmptyContainers(t *testing.T) {
	var buf bytes.Buffer
	if err := reportKill(&buf, &serverv1.KillResponse{}, true); err != nil {
		t.Fatalf("reportKill() error = %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(buf.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if raw["killed"] == nil {
		t.Error("killed is null, want an empty array")
	}
	if raw["errors"] == nil {
		t.Error("errors is null, want an empty object")
	}
}

// `cm info --field busy` and `--field command` are what a terminal emulator reads to decide whether
// closing a window would interrupt something.
//
// Asserted specifically because these are the interface a close-confirmation hook depends on, and a
// bare value with no header or padding is the point: a kitten should not have to parse anything.
func TestPrintSessionInfoReportsBusyFields(t *testing.T) {
	for _, tc := range []struct {
		field string
		want  string
	}{
		{field: "busy", want: "true\n"},
		{field: "command", want: "nvim notes.md\n"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printSessionInfo(&buf, sampleWireSession("work"), tc.field); err != nil {
				t.Fatalf("printSessionInfo() error = %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("--field %s = %q, want %q", tc.field, got, tc.want)
			}
		})
	}
}

// An idle session must report false rather than nothing, so a caller can tell "not busy" from a
// failed lookup.
func TestPrintSessionInfoBusyIsFalseWhenIdle(t *testing.T) {
	s := sampleWireSession("idle")
	s.Busy = false
	s.Command = ""

	var buf bytes.Buffer
	if err := printSessionInfo(&buf, s, "busy"); err != nil {
		t.Fatalf("printSessionInfo() error = %v", err)
	}
	if got := buf.String(); got != "false\n" {
		t.Errorf("--field busy on an idle session = %q, want %q", got, "false\n")
	}
}

// The table shows what a session is running, since that is the question a list is usually asked.
//
// In the state column rather than a new one: a seventh column would push CWD off a normal terminal,
// and "running(nvim)" reads the same way as the existing "exited(0)".
func TestSessionsTableShowsTheRunningCommand(t *testing.T) {
	for _, tc := range []struct {
		name    string
		busy    bool
		command string
		want    string
	}{
		// The program name only: a full command line would wreck the table, and `cm info` has it whole.
		{name: "running a command", busy: true, command: "nvim notes.md", want: "running(nvim)"},
		// Busy but the shell did not say what, which happens with a shell that sends a bare 133;C.
		{name: "busy with no command reported", busy: true, command: "", want: "running(busy)"},
		{name: "idle", busy: false, command: "", want: "running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleWireSession("work")
			s.Busy = tc.busy
			s.Command = tc.command
			// No report, so the derived state is what shows. A report would take precedence, which the
			// test below covers.
			s.ReportedState = ""
			s.ReportedDetail = ""

			var buf bytes.Buffer
			if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
				t.Fatalf("printSessionsTable() error = %v", err)
			}
			if !strings.Contains(buf.String(), tc.want) {
				t.Errorf("table = %q, want it to contain %q", buf.String(), tc.want)
			}
		})
	}
}

// A reported state is shown in preference to the derived one.
//
// The precedence has to be visible where a person looks, not only where a script does: a session reported
// blocked while its shell says a command is running should read as blocked, since that is the part the user
// can act on.
func TestSessionsTablePrefersAReportedState(t *testing.T) {
	s := sampleWireSession("work")
	// The shell's view and the program's view disagree, which is the normal case for an agent: one
	// long-running command from the shell's side, several states from the program's.
	s.Busy = true
	s.Command = "claude"
	s.ReportedState = "blocked"
	s.ReportedDetail = "needs approval"

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "running(blocked: needs approval)") {
		t.Errorf("table = %q, want the reported state and its detail", got)
	}
	if strings.Contains(got, "running(claude)") {
		t.Errorf("table = %q, want the report to take precedence over the derived command", got)
	}
}

// The NAME column shows every name, not just the label.
//
// This is the whole point of a session answering to several. An alias bound onto a session the terminal
// emulator named resolves fine for attach, send and kill, but the label is the first name, so the listing
// said "kitty.325" and finding the session by what it was working on still meant grepping.
func TestSessionsTableShowsEveryName(t *testing.T) {
	s := sampleWireSession("kitty.325")
	s.Names = []string{"kitty.325", "refactor"}

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "kitty.325,refactor") {
		t.Errorf("table = %q, want both names in the NAME column", got)
	}
}

// A session nothing names still fills the cell, because the server puts an ID reference in Name.
//
// The cell must never be blank: what NAME shows is what a reader types into the next command, and a
// session with no names is reachable only by ID.
func TestSessionsTableNamesASessionWithNoBindings(t *testing.T) {
	s := sampleWireSession("@a7k2m9x4")
	s.Names = nil

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "@a7k2m9x4") {
		t.Errorf("table = %q, want the ID reference in the NAME column", got)
	}
}

// A long detail is truncated rather than reduced to its first word.
//
// A detail is a sentence a human wrote, so "needs approval to write a file" must not become "needs". The
// column still has to stay readable, and the full value is in `cm info` and the JSON.
func TestSessionsTableTruncatesALongDetail(t *testing.T) {
	s := sampleWireSession("work")
	s.ReportedState = "blocked"
	s.ReportedDetail = "needs approval to write a file somewhere under /etc"

	var buf bytes.Buffer
	if err := printSessionsTable(&buf, []*serverv1.Session{s}); err != nil {
		t.Fatalf("printSessionsTable() error = %v", err)
	}
	got := buf.String()
	// Cut, and marked as cut, rather than silently ending mid-word.
	if !strings.Contains(got, "...") {
		t.Errorf("table = %q, want a long detail marked as truncated", got)
	}
	// The beginning survives, which is the part that carries meaning.
	if !strings.Contains(got, "needs approval") {
		t.Errorf("table = %q, want the start of the detail kept", got)
	}
	if strings.Contains(got, "/etc") {
		t.Errorf("table = %q, want the tail of a long detail dropped", got)
	}
}

// Every printable field is accepted by --field, and the flag's help lists them all.
//
// The two drifted before: the help named eight fields while the printer accepted sixteen, so busy, command,
// and the reported_* trio worked and were undocumented. Both now derive from one list, and this asserts they
// stay in step -- a field that prints but is not documented is one nobody finds.
func TestSessionFieldNamesMatchWhatIsAccepted(t *testing.T) {
	s := sampleWireSession("work")

	names := SessionFieldNames()
	if len(names) == 0 {
		t.Fatal("SessionFieldNames() is empty")
	}

	for _, name := range names {
		var buf bytes.Buffer
		if err := printSessionInfo(&buf, s, name); err != nil {
			t.Errorf("printSessionInfo(field=%q) error = %v, but the help lists it", name, err)
		}
		if buf.Len() == 0 {
			t.Errorf("field %q printed nothing", name)
		}
	}

	// And the table lists exactly those fields, so nothing prints in one mode and not the other.
	var table bytes.Buffer
	if err := printSessionInfo(&table, s, ""); err != nil {
		t.Fatalf("printSessionInfo() error = %v", err)
	}
	for _, name := range names {
		if !bytes.Contains(table.Bytes(), []byte(name)) {
			t.Errorf("the table output is missing field %q:\n%s", name, table.String())
		}
	}
}

// A session hosting a nested attach says so where a person is looking.
//
// The annotation is what makes the frozen directory readable. While a session hosts a nested attach its
// shell is blocked inside `cm attach` and reports nothing, so the path shown is where it was when the
// nesting began. Without the note that is indistinguishable from a stale value, which is exactly what
// the underlying bug used to make it: the parent showed the *child's* directory as its own.
func TestSessionsTableMarksASessionHostingANestedAttach(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cwd     string
		hosting []string
		want    string
	}{
		{
			name:    "hosting one session",
			cwd:     "/home/user/projects",
			hosting: []string{"inner"},
			want:    "/home/user/projects  (hosting inner)",
		},
		// Several at once, which happens when the thing running inside has panes of its own.
		{
			name:    "hosting several",
			cwd:     "/home/user/projects",
			hosting: []string{"a", "b"},
			want:    "/home/user/projects  (hosting a b)",
		},
		// A session that never reported a directory still shows why, rather than an empty cell.
		{
			name:    "hosting with no directory reported",
			cwd:     "",
			hosting: []string{"inner"},
			want:    "(hosting inner)",
		},
		{
			name:    "not hosting anything",
			cwd:     "/home/user/projects",
			hosting: nil,
			want:    "/home/user/projects",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleWireSession("work")
			s.Cwd = tc.cwd
			s.Hosting = tc.hosting

			if got := sessionCwdColumn(s); got != tc.want {
				t.Errorf("sessionCwdColumn() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A path under home abbreviates to "~/...", the way a shell prompt writes it.
//
// Width is the reason: CWD sits last, so every column before it eats into the path, and a home prefix is
// the longest part of a path that carries the least information. The cases below are the ones that turn a
// shortener into a liar.
func TestShortenHome(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		home string
		want string
	}{
		{name: "under home", path: "/home/user/projects/cm", home: "/home/user", want: "~/projects/cm"},
		{name: "home itself", path: "/home/user", home: "/home/user", want: "~"},
		// A sibling whose name merely starts with home's must be left alone. Matching without the
		// separator would render this as "~2", which names a directory that does not exist.
		{name: "sibling sharing a prefix", path: "/home/user2/projects", home: "/home/user", want: "/home/user2/projects"},
		{name: "not under home", path: "/var/log", home: "/home/user", want: "/var/log"},
		// A trailing slash on HOME would make the prefix "//", so it is trimmed rather than trusted.
		{name: "home with a trailing slash", path: "/home/user/projects", home: "/home/user/", want: "~/projects"},
		// Home as root would abbreviate every absolute path to "~", which hides the path rather than
		// shortening it.
		{name: "home is root", path: "/var/log", home: "/", want: "/var/log"},
		{name: "no home", path: "/var/log", home: "", want: "/var/log"},
		{name: "no path", path: "", home: "/home/user", want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shortenHome(tc.path, tc.home); got != tc.want {
				t.Errorf("shortenHome(%q, %q) = %q, want %q", tc.path, tc.home, got, tc.want)
			}
		})
	}
}

// The CWD column abbreviates home, and only for a directory on this machine.
//
// A remote session's home belongs to the remote user, so rewriting it against this machine's would claim
// a relationship that does not exist: the path only looks like it is under home because both hosts put
// users in /home. Asserted through the column rather than shortenHome alone, since the locality check is
// the part that lives here.
func TestSessionCwdColumnAbbreviatesLocalHomeOnly(t *testing.T) {
	// os.UserHomeDir reads HOME on both platforms cm supports.
	t.Setenv("HOME", "/home/user")

	for _, tc := range []struct {
		name    string
		cwd     string
		isLocal bool
		want    string
	}{
		{name: "local under home", cwd: "/home/user/projects", isLocal: true, want: "~/projects"},
		{name: "local outside home", cwd: "/var/log", isLocal: true, want: "/var/log"},
		{
			name:    "remote path that resembles a home path",
			cwd:     "/home/user/projects",
			isLocal: false,
			want:    "/home/user/projects (remote)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sampleWireSession("work")
			s.Cwd = tc.cwd
			s.CwdIsLocal = tc.isLocal

			if got := sessionCwdColumn(s); got != tc.want {
				t.Errorf("sessionCwdColumn() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The abbreviation is for the table only. The JSON output and `cm info --field cwd` stay absolute.
//
// Those are read by scripts that cd into the value or hand it to a terminal emulator opening a window
// there, and "~" only expands in a shell: a Go or Python caller would create a directory named "~".
func TestCwdStaysAbsoluteOutsideTheTable(t *testing.T) {
	t.Setenv("HOME", "/home/user")

	s := sampleWireSession("work")
	s.Cwd = "/home/user/projects"

	if got := toSessionJSON(s).Cwd; got != "/home/user/projects" {
		t.Errorf("JSON cwd = %q, want the absolute path a script can act on", got)
	}

	var buf bytes.Buffer
	if err := printSessionInfo(&buf, s, "cwd"); err != nil {
		t.Fatalf("printSessionInfo(field=cwd) error = %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "/home/user/projects" {
		t.Errorf("cm info --field cwd = %q, want the absolute path", got)
	}
}

// The remote marker and the hosting note have to coexist.
//
// Both qualify the same cell, and a session that ssh'd somewhere and then hosted a nested attach is not
// hypothetical: it is how an agent session driving another one on a remote host looks.
func TestSessionsTableMarksRemoteAndHostingTogether(t *testing.T) {
	s := sampleWireSession("work")
	s.Cwd = "/remote/path"
	s.CwdIsLocal = false
	s.Hosting = []string{"inner"}

	want := "/remote/path (remote)  (hosting inner)"
	if got := sessionCwdColumn(s); got != want {
		t.Errorf("sessionCwdColumn() = %q, want %q", got, want)
	}
}

// The STATE column shows a report's age only once the age changes what the state means.
//
// A program reports on each change, so a recent report is the current state and the age would only cost
// width. An old one is either a program blocked for hours or a report that survived a server restart, and
// both are cases where "blocked" alone reads as more current than it is.
func TestReportAge(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) int64 { return now.Add(-d).Unix() }

	tests := []struct {
		name string
		unix int64
		want string
	}{
		{"nothing reported", 0, ""},
		{"a minute ago", ago(time.Minute), ""},
		{"just under the threshold", ago(59 * time.Minute), ""},
		{"an hour ago", ago(time.Hour), "1h"},
		{"three hours ago", ago(3 * time.Hour), "3h"},
		// Hours up to two days, so "36h" reads as one working day rather than rounding to "1d".
		{"36 hours ago", ago(36 * time.Hour), "36h"},
		{"three days ago", ago(72 * time.Hour), "3d"},
		// A clock that moved, or a record from a machine whose clock differs. Not "-2h".
		{"timestamped in the future", now.Add(2 * time.Hour).Unix(), ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reportAge(tc.unix, now); got != tc.want {
				t.Errorf("reportAge() = %q, want %q", got, tc.want)
			}
		})
	}
}

// The age reaches the column, after the detail, so a listing shows why a report should not be trusted as
// current.
func TestSessionStateColumnShowsAnOldReportsAge(t *testing.T) {
	s := sampleWireSession("work")
	s.ReportedState = "blocked"
	s.ReportedDetail = "needs approval"
	s.ReportedAtUnix = time.Now().Add(-3 * time.Hour).Unix()

	want := "running(blocked: needs approval, 3h)"
	if got := sessionStateColumn(s); got != want {
		t.Errorf("sessionStateColumn() = %q, want %q", got, want)
	}
}

// An instant nobody recorded marshals as null, with its key still present.
//
// The rule the whole timestamp shape rests on, and the one a change would break silently. A time.Time
// renders its zero value as a real date, so a field that must be able to say nothing has to be a pointer;
// and omitempty on that pointer would drop the key entirely, leaving a script to handle absence as well as
// null. Asserted on the bytes rather than the struct, because both failures are invisible from the Go side.
func TestJSONTimestampsAreNullWhenUnset(t *testing.T) {
	s := sampleWireSession("work")
	s.ReportedAtUnix = 0
	s.AttachedClients = []*serverv1.AttachedClient{{Pid: 5150}}

	var buf bytes.Buffer
	if err := writeJSON(&buf, toSessionJSON(s)); err != nil {
		t.Fatalf("writeJSON() error = %v", err)
	}
	got := buf.String()

	for _, want := range []string{`"reported_at": null`, `"attached_at": null`} {
		if !strings.Contains(got, want) {
			t.Errorf("output is missing %s:\n%s\n"+
				"An unset instant has to be null. A zero time.Time renders as a date decades ago, which a "+
				"reader acts on, and omitempty removes the key instead of reporting nothing.", want, got)
		}
	}
	// A set one renders as RFC 3339 seconds in the local zone. Nanoseconds would appear if anything ever
	// built one of these from a clock rather than from the wire's unix seconds, and the sub-second digits
	// would then vary per run.
	if !strings.Contains(got, `"created_at": "`) || strings.Contains(got, `.000000`) {
		t.Errorf("created_at is not a plain RFC 3339 timestamp:\n%s", got)
	}
}
