package server

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// replayTerminal is a fake emulator that records what it was fed, so replay can be tested without
// the cgo terminal.
func replayTerminal(t *testing.T) (NewTerminalFunc, *fakeTerminal) {
	t.Helper()
	term := &fakeTerminal{restore: []byte("REPLAYED_SCREEN")}
	return func(rows, cols uint16) (Terminal, error) {
		term.Resize(rows, cols)
		return term, nil
	}, term
}

// The property reboot persistence rests on: a saved log becomes a screen.
func TestReplayPersistedRebuildsScreen(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "session.log")

	f, err := seqlog.OpenFile[seq.Shim](path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if err := f.Append([]byte("line one\r\nline two\r\n")); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	_, wantNext := f.Bounds()
	f.Close()

	newTerm, term := replayTerminal(t)
	restore, next, err := replayPersisted(path, newTerm, 24, 80, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("replayPersisted() error = %v", err)
	}

	if string(restore) != "REPLAYED_SCREEN" {
		t.Errorf("restore = %q, want the serialized screen", restore)
	}
	// The saved bytes must actually reach the emulator, or the screen would be empty.
	if got := term.Written(); !strings.Contains(got, "line one") || !strings.Contains(got, "line two") {
		t.Errorf("emulator saw %q, want the persisted output", got)
	}
	// The resume position must continue the log's numbering, so a sequence number still means the
	// same byte after a reboot.
	if next != wantNext {
		t.Errorf("next = %d, want %d", next, wantNext)
	}
}

// The replay terminal must be sized for the client that will display it, or the screen would be
// laid out for the wrong width.
func TestReplayPersistedUsesRequestedSize(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "session.log")
	f, err := seqlog.OpenFile[seq.Shim](path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	f.Append([]byte("content\r\n"))
	f.Close()

	newTerm, term := replayTerminal(t)
	if _, _, err := replayPersisted(path, newTerm, 40, 120, seqlog.FileLimits{}); err != nil {
		t.Fatalf("replayPersisted() error = %v", err)
	}
	if rows, cols := term.Size(); rows != 40 || cols != 120 {
		t.Errorf("replay terminal size = (%d, %d), want (40, 120)", rows, cols)
	}
}

// Query responses generated during replay must be discarded. They answer questions a program asked
// before the reboot, and that program is gone; delivering them would inject stray input into a new
// shell.
func TestReplayPersistedDiscardsGeneratedInput(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "session.log")
	f, err := seqlog.OpenFile[seq.Shim](path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	f.Append([]byte("content\r\n"))
	f.Close()

	term := &fakeTerminal{
		restore: []byte("SCREEN"),
		pending: [][]byte{[]byte("\x1b[0n")},
	}
	newTerm := func(rows, cols uint16) (Terminal, error) { return term, nil }

	if _, _, err := replayPersisted(path, newTerm, 24, 80, seqlog.FileLimits{}); err != nil {
		t.Fatalf("replayPersisted() error = %v", err)
	}
	if len(term.TakePending()) != 0 {
		t.Error("replay left generated input queued, which would reach a new shell as keystrokes")
	}
}

func TestReplayPersistedMissingCases(t *testing.T) {
	newTerm, _ := replayTerminal(t)
	dir := shortTempDir(t)

	tests := []struct {
		name string
		path string
		nt   NewTerminalFunc
	}{
		{"no path", "", newTerm},
		{"missing file", filepath.Join(dir, "absent.log"), newTerm},
		// Without an emulator the bytes cannot become a screen, and replaying them raw would dump
		// the whole log into the terminal.
		{"no terminal", filepath.Join(dir, "absent.log"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := replayPersisted(tt.path, tt.nt, 24, 80, seqlog.FileLimits{})
			if !errors.Is(err, ErrNothingToRestore) {
				t.Errorf("error = %v, want ErrNothingToRestore", err)
			}
		})
	}
}

// An empty log is nothing to restore rather than an error, since a session that persisted but
// produced no output is a normal thing to encounter.
func TestReplayPersistedEmptyLog(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "empty.log")
	f, err := seqlog.OpenFile[seq.Shim](path, seqlog.FileLimits{})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	f.Close()

	newTerm, _ := replayTerminal(t)
	_, _, err = replayPersisted(path, newTerm, 24, 80, seqlog.FileLimits{})
	if !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("error = %v, want ErrNothingToRestore for an empty log", err)
	}
}

// A trimmed log still replays: the retained tail is what rebuilds a screen, and the resume position
// must reflect the trimmed numbering rather than starting from zero.
func TestReplayPersistedAfterTrim(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "trimmed.log")
	f, err := seqlog.OpenFile[seq.Shim](path, seqlog.FileLimits{MaxLines: 2})
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	for _, line := range []string{"dropped\r\n", "kept one\r\n", "kept two\r\n"} {
		if err := f.Append([]byte(line)); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	oldest, wantNext := f.Bounds()
	f.Close()

	if oldest == 0 {
		t.Fatal("log was not trimmed, so this test would not exercise the case")
	}

	newTerm, term := replayTerminal(t)
	_, next, err := replayPersisted(path, newTerm, 24, 80, seqlog.FileLimits{MaxLines: 2})
	if err != nil {
		t.Fatalf("replayPersisted() error = %v", err)
	}
	if next != wantNext {
		t.Errorf("next = %d, want %d: the resume position must follow the trimmed numbering",
			next, wantNext)
	}
	if got := term.Written(); strings.Contains(got, "dropped") {
		t.Errorf("emulator saw trimmed content: %q", got)
	}
}
