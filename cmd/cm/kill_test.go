package main

import (
	"bytes"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// --force and --keep are refused together rather than ranked.
//
// force's second half is "forget the session even if its shim cannot be reached" and keep says not to
// forget it, so one of the two was a mistake. Refused before the request goes out, so the server never has
// to decide which half of a contradiction to honor.
func TestKillCommandRejectsForceWithKeep(t *testing.T) {
	cmd := newKillCommand(&globals{})
	cmd.SetArgs([]string{"--force", "--keep", "work"})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("Execute() = nil error, want a refusal")
	}
	if !strings.Contains(err.Error(), "cannot be combined") {
		t.Errorf("error = %q, want it to say the two cannot be combined", err)
	}
}

// --expire-after is refused without --keep, and with a value that is not a positive duration or never, before
// any request goes out.
//
// Without --keep the record is forgotten at once, so a retention has nothing to apply to; reaching the server
// would kill the session and silently drop the half of the request that said to keep it.
func TestKillCommandValidatesExpireAfter(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--expire-after", "never", "work"}, "--expire-after needs --keep; without it the session is forgotten now"},
		{[]string{"--force", "--expire-after", "never", "work"}, "--expire-after needs --keep; without it the session is forgotten now"},
		{[]string{"--keep", "--expire-after", "0s", "work"}, `--expire-after: expiry must be positive or never, got "0s"`},
		{[]string{"--keep", "--expire-after", "forever", "work"}, `--expire-after: expiry "forever": want a Go duration such as 720h, or never`},
		{[]string{"--keep", "--expire-after", "", "work"}, `--expire-after: expiry "": want a Go duration such as 720h, or never`},
	} {
		cmd := newKillCommand(&globals{})
		cmd.SetArgs(tc.args)
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))

		if err := cmd.Execute(); err == nil || err.Error() != tc.want {
			t.Errorf("Execute(%q) error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// A partly successful forget prints what went and fails, so a script sees both.
//
// The whole output is asserted rather than a substring: what a caller reads is every line together, and a
// check for one of them passes while another is missing or duplicated.
func TestReportForgetPrintsBothOutcomes(t *testing.T) {
	var buf bytes.Buffer
	err := reportForget(&buf, &serverv1.ForgetResponse{
		Forgotten: []string{"done", "@a7k2m9x4"},
		Errors:    map[string]string{"busy": "busy is still running; use `cm kill` to stop it first"},
	}, false)

	if want := "forgot done\nforgot @a7k2m9x4\n"; buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
	// Non-nil so the exit status is non-zero, and it names the session and the reason.
	want := "busy: busy is still running; use `cm kill` to stop it first"
	if err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
}

// JSON carries both halves in the payload, with the empty ones spelled out.
//
// null for a missing list is the trap: a script iterating .forgotten would break on a call where
// everything was refused, which is precisely the call it has to handle.
func TestReportForgetJSONIsAContract(t *testing.T) {
	var buf bytes.Buffer
	err := reportForget(&buf, &serverv1.ForgetResponse{}, true)
	if err != nil {
		t.Errorf("error = %v, want nil with nothing refused", err)
	}
	if want := "{\n  \"forgotten\": [],\n  \"errors\": {}\n}\n"; buf.String() != want {
		t.Errorf("output = %q, want %q", buf.String(), want)
	}
}
