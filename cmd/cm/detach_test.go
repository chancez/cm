package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// The whole result is asserted rather than a few fields, per the project's testing rules: a
// field-by-field check passes while the rest is wrong.
func TestReportDetachJSON(t *testing.T) {
	resp := &serverv1.DetachResponse{
		Detached: map[string]uint32{"busy": 2, "idle": 0},
		Errors:   map[string]string{"gone": "session not found"},
	}

	var buf bytes.Buffer
	err := reportDetach(&buf, resp, true)
	// JSON mode carries the detail in the payload, so the error only sets the exit status. Matching
	// reportKill rather than inventing a second convention.
	if !errors.Is(err, errAlreadyReported) {
		t.Errorf("error = %v, want errAlreadyReported so the payload is not duplicated to stderr", err)
	}

	var got []detachJSON
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output does not unmarshal: %v\n%s", err, buf.String())
	}
	// Sorted by name, so a script diffing output sees no spurious change between runs.
	want := []detachJSON{
		{Session: "busy", Clients: 2},
		{Session: "gone", Error: "session not found"},
		{Session: "idle", Clients: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("payload = %+v\nwant %+v", got, want)
	}
}

// A session with nothing attached is reported as such rather than as a failure.
//
// The property that makes `cm detach` safe to call without checking first: a teardown script asking an
// idle session to detach has already got what it wanted.
func TestReportDetachIdleSessionIsNotAnError(t *testing.T) {
	resp := &serverv1.DetachResponse{Detached: map[string]uint32{"idle": 0}}

	var buf bytes.Buffer
	if err := reportDetach(&buf, resp, false); err != nil {
		t.Errorf("error = %v, want nil: detaching a session with no clients is already satisfied", err)
	}
	if !strings.Contains(buf.String(), "no clients attached") {
		t.Errorf("output %q should say nothing was attached rather than reporting 0 detached",
			buf.String())
	}
}

// A partial failure detaches the rest and still exits non-zero.
//
// Both halves matter. `cm detach --all` against a server holding one bad record should release every
// other session, and a script must be able to notice from the exit status without parsing output.
func TestReportDetachPartialFailure(t *testing.T) {
	resp := &serverv1.DetachResponse{
		Detached: map[string]uint32{"ok": 1},
		Errors:   map[string]string{"bad": "session not found"},
	}

	var buf bytes.Buffer
	err := reportDetach(&buf, resp, false)
	if err == nil {
		t.Error("error = nil with a failed session, want one so the exit status is non-zero")
	}
	if err != nil && !strings.Contains(err.Error(), "session not found") {
		t.Errorf("error %q does not say why", err)
	}
	// The successful one is still reported, so the output is not just the failure.
	if !strings.Contains(buf.String(), "ok") || !strings.Contains(buf.String(), "detached") {
		t.Errorf("output %q should report the session that did detach", buf.String())
	}
}

// --all and --tag cannot be combined, and neither takes session names.
//
// Refused rather than resolved, matching kill: --all means every session and --tag means a subset, so
// one of the two was a mistake and guessing which would detach too much or too little.
func TestDetachCommandRejectsConflictingSelectors(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "all with a tag",
			args: []string{"--all", "--tag", "project=cm"},
			want: "cannot be combined",
		},
		{
			name: "all with a session name",
			args: []string{"--all", "work"},
			want: "take no session names",
		},
		{
			name: "tag with a session name",
			args: []string{"--tag", "project=cm", "work"},
			want: "take no session names",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := newDetachCommand(&globals{})
			cmd.SetArgs(tc.args)
			cmd.SetOut(new(bytes.Buffer))
			cmd.SetErr(new(bytes.Buffer))

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) = nil error, want one", tc.args)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
