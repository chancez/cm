package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
	}
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
		"name", "state", "shell_pid", "clients", "exit_code", "title",
		"cwd", "cwd_uri", "cwd_is_local", "created_at", "created_at_unix",
		"busy", "command",
		"reported_state", "reported_detail", "reported_source",
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
		CreatedAt:      "2023-11-14T14:13:20-08:00",
		CreatedAtUnix:  1_700_000_000,
		Busy:           true,
		Command:        "nvim notes.md",
		ReportedState:  "blocked",
		ReportedDetail: "needs approval",
		ReportedSource: "my-agent",
	}
	// CreatedAt renders in the local zone, so compare it separately rather than pinning a zone.
	want.CreatedAt = got.CreatedAt
	if got != want {
		t.Errorf("toSessionJSON() = %+v\nwant %+v", got, want)
	}
	if !strings.HasPrefix(got.CreatedAt, "2023-11-14") && !strings.HasPrefix(got.CreatedAt, "2023-11-15") {
		t.Errorf("CreatedAt = %q, want an RFC 3339 timestamp for the given instant", got.CreatedAt)
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
