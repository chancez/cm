package server

import (
	"errors"
	"fmt"
	"os"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// ErrNothingToRestore reports that a session left no content worth replaying.
var ErrNothingToRestore = errors.New("session has no persisted content")

// replayPersisted rebuilds a screen from a session's saved output log.
//
// This is what makes a session survive a reboot. The log holds the raw bytes the pty produced, so
// feeding them through a terminal emulator reconstructs the screen and scrollback exactly as the
// last client saw them, and serializing that gives a restore blob indistinguishable from a live
// session's.
//
// Returns the blob and the sequence number the log ends at, so a client resuming later has a
// position that means the same thing as it did before the reboot.
func replayPersisted(
	logPath string,
	newTerminal NewTerminalFunc,
	rows, cols uint16,
	limits seqlog.FileLimits,
) (restore []byte, next seq.Shim, err error) {
	if logPath == "" {
		return nil, 0, ErrNothingToRestore
	}
	if _, statErr := os.Stat(logPath); statErr != nil {
		// A missing log is the normal case for a session that never persisted, so it is not an
		// error worth surfacing as one.
		return nil, 0, ErrNothingToRestore
	}
	if newTerminal == nil {
		// Without an emulator the bytes cannot be turned into a screen. Replaying them raw would
		// dump an entire log into the terminal, so refusing is better.
		return nil, 0, ErrNothingToRestore
	}

	f, err := seqlog.OpenFile[seq.Shim](logPath, limits)
	if err != nil {
		return nil, 0, fmt.Errorf("opening persisted log: %w", err)
	}
	defer f.Close()

	oldest, end := f.Bounds()
	if end == oldest {
		return nil, 0, ErrNothingToRestore
	}

	data, _, err := f.ReadFrom(oldest)
	if err != nil {
		return nil, 0, fmt.Errorf("reading persisted log: %w", err)
	}
	if len(data) == 0 {
		return nil, 0, ErrNothingToRestore
	}

	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	term, err := newTerminal(rows, cols)
	if err != nil {
		return nil, 0, fmt.Errorf("creating terminal for replay: %w", err)
	}
	if term == nil {
		return nil, 0, ErrNothingToRestore
	}
	defer term.Close()

	// One write rather than chunked: the emulator's parser is stateful across calls anyway, and a
	// single call avoids any question of a sequence being split at a boundary.
	if err := term.Write(data); err != nil {
		return nil, 0, fmt.Errorf("replaying persisted log: %w", err)
	}

	// Anything the emulator generated in response is discarded. Those are answers to queries the
	// program asked before the reboot, and that program no longer exists; delivering them to a new
	// shell would inject stray input.
	term.TakePending()

	restore, err = term.Restore()
	if err != nil {
		return nil, 0, fmt.Errorf("serializing replayed state: %w", err)
	}
	return restore, end, nil
}
