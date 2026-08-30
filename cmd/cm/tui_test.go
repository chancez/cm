package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/tui"
)

// TestTheAttachmentArgvCarriesThisPickersDirectories is the isolation test.
//
// The attachment is a child process, so it resolves its own directories and config unless told
// otherwise, and it would then talk to a different server from the one the picker is listing. In a
// sandbox that means attaching to the developer's real sessions from a picker that looks isolated,
// which is the failure AGENTS.md's isolation rule exists to prevent.
//
// The whole argv is asserted rather than checked for the flags, because the order matters: `cm attach`
// takes the reference last, and a global flag after the subcommand is a different command line.
func TestTheAttachmentArgvCarriesThisPickersDirectories(t *testing.T) {
	argv := attachArgv(&globals{
		runtimeDir: "/tmp/r",
		stateDir:   "/tmp/s",
		configPath: "/nonexistent.toml",
	}, false)

	want := []string{
		"--runtime-dir", "/tmp/r",
		"--state-dir", "/tmp/s",
		"--config", "/nonexistent.toml",
		"attach", "@a7k2m9x4",
	}
	if got := argv("@a7k2m9x4"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q, want %q", got, want)
	}
}

// TestAnUnsetFlagIsNotForwarded keeps a resolved default out of the child's argv.
//
// Forwarding what this process resolved rather than what it was given would write a directory into the
// child's command line as though it had been asked for, and that argv is what `ps` reports for as long
// as the attachment lasts. It also makes the common case, with nothing set, the one that carries three
// flags nobody typed.
func TestAnUnsetFlagIsNotForwarded(t *testing.T) {
	want := []string{"attach", "work"}
	if got := attachArgv(&globals{}, false)("work"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q, want %q", got, want)
	}
}

// TestANewSessionPassesNoReference covers what the picker's "new session" means.
//
// `cm attach` with no name asks the server to allocate one. An empty string appended as an argument
// instead would be a session reference that is empty, which is a different request and an error.
func TestANewSessionPassesNoReference(t *testing.T) {
	want := []string{"attach"}
	if got := attachArgv(&globals{}, false)(""); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q, want %q", got, want)
	}
}

func TestReadOnlyIsForwarded(t *testing.T) {
	want := []string{"attach", "--read-only", "work"}
	if got := attachArgv(&globals{}, true)("work"); !reflect.DeepEqual(got, want) {
		t.Errorf("argv %q, want %q", got, want)
	}
}

// TestWhatTheChildPrintedBecomesTheNote is why the child's stderr is captured.
//
// `cm attach` reports how the attachment ended on stderr. Left alone, those bytes land on a screen
// bubbletea is about to repaint from its own model, so the message is either erased or spliced into the
// frame. Captured, the same text is what the status line says, in the wording `cm attach` already uses.
func TestWhatTheChildPrintedBecomesTheNote(t *testing.T) {
	var ran []string
	attach := attachFromPicker(
		attachArgv(&globals{}, false),
		func(_ context.Context, argv []string) (string, error) {
			ran = argv
			return "detached from work", nil
		},
	)

	got, err := attach(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if want := (tui.Attachment{Note: "detached from work"}); got != want {
		t.Errorf("attachment %+v, want %+v", got, want)
	}
	if want := []string{"attach", "work"}; !reflect.DeepEqual(ran, want) {
		t.Errorf("ran %q, want %q", ran, want)
	}
}

// TestAFailedAttachmentReportsAnError checks the picker learns about a child that could not attach.
//
// Reported rather than swallowed into the status line as though it were an outcome: a stale reference,
// which is what a row for a session that has since been killed produces, has to read as a failure.
func TestAFailedAttachmentReportsAnError(t *testing.T) {
	failed := errors.New("no session @a7k2m9x4")
	attach := attachFromPicker(
		attachArgv(&globals{}, false),
		func(context.Context, []string) (string, error) { return "", failed },
	)

	got, err := attach(context.Background(), "@a7k2m9x4")
	if !errors.Is(err, failed) {
		t.Errorf("error %v, want %v", err, failed)
	}
	if want := (tui.Attachment{}); got != want {
		t.Errorf("attachment %+v, want %+v: a failure has nothing to report about the session", got, want)
	}
}
