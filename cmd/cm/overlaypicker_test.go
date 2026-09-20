package main

import (
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/client"
)

// The picker is told which key gets it back here, so the way out of a session and the way back are the
// same key. Derived from what this client intercepts rather than from the config file, which is the
// difference that matters for a client given --prefix-key on the command line.
//
// The whole argv, since the order is part of it: a global flag after the subcommand is a different command
// line, and the directory flags are what keep a sandboxed client's picker off the real server.
func TestThePickerArgvCarriesTheKeyThatComesBack(t *testing.T) {
	prefix, err := client.ParsePrefixKey("ctrl-o")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"--runtime-dir", "/tmp/r",
		"tui", "--chosen-file", "/tmp/r/picked-1",
		"--back-key", "ctrl-o",
	}
	got := pickerArgv(&globals{runtimeDir: "/tmp/r"}, "/tmp/r/picked-1", pickerBackKey(prefix))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q, want %q", got, want)
	}
}

// A prefix key turned off leaves the flag out rather than passing "none". There is then no way to open the
// overlay, so nothing can reach the picker from a session, and a picker offering the key would be naming
// one that cannot be pressed.
func TestADisabledPrefixKeyOffersNoWayBack(t *testing.T) {
	prefix, err := client.ParsePrefixKey("none")
	if err != nil {
		t.Fatal(err)
	}
	if got := pickerBackKey(prefix); got != "" {
		t.Errorf("back key %q, want none", got)
	}

	want := []string{"tui", "--chosen-file", "/tmp/picked-1"}
	if got := pickerArgv(&globals{}, "/tmp/picked-1", pickerBackKey(prefix)); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q, want %q", got, want)
	}
}
