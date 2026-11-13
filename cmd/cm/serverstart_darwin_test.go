package main

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"reflect"
	"testing"
)

// The plist must reach launchd saying exactly what was written, for any path a user can have.
//
// Worth a test because the failure is silent: a plist launchd refuses makes startServer fall back to starting
// the server directly, which works, and leaves the server in the caller's coalition where a Force Quit in the
// Dock can take every session. Nothing would report it.
func TestServerJobPlistSaysWhatWasWritten(t *testing.T) {
	argv := []string{"/Users/a b/&<x>\"y\"'z'/caf\u00e9/\u65e5\u672c/cm", "--runtime-dir", "/tmp/r", "server", "spawn", "/tmp/r/e.req"}
	plist, err := serverJobPlist("com.example.label", argv, "/tmp/r/server-start.err")
	if err != nil {
		t.Fatalf("serverJobPlist() error = %v", err)
	}

	cmd := exec.Command("plutil", "-convert", "json", "-o", "-", "-")
	cmd.Stdin = bytes.NewReader(plist)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("plutil rejected the plist: %v\n%s", err, plist)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("parsing plutil output %q: %v", out, err)
	}

	want := map[string]any{
		"Label":             "com.example.label",
		"ProgramArguments":  []any{argv[0], argv[1], argv[2], argv[3], argv[4], argv[5]},
		"StandardErrorPath": "/tmp/r/server-start.err",
		"ProcessType":       "Interactive",
		"RunAtLoad":         true,
		"KeepAlive":         false,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("plist decodes to %v, want %v", got, want)
	}
}

// A value XML cannot carry is refused rather than altered.
//
// xml.EscapeText replaces such a character instead of failing, so without the check launchd would be handed a
// different path and run something else, or nothing.
func TestServerJobPlistRefusesWhatXMLCannotCarry(t *testing.T) {
	for _, arg := range []string{"/tmp/\x1b[31m/cm", "/tmp/\xff/cm"} {
		if _, err := serverJobPlist("com.example.label", []string{arg}, "/tmp/err"); err == nil {
			t.Errorf("serverJobPlist(%q) error = nil, want a refusal", arg)
		}
	}
}
