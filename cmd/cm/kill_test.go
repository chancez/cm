package main

import (
	"bytes"
	"strings"
	"testing"
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
