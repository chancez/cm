package server

import (
	"errors"
	"fmt"
	"os"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// ErrNothingToRestore reports that a session left no content worth replaying.
var ErrNothingToRestore = errors.New("session has no persisted content")

// seedFromPersistedLog rebuilds a revived session's screen inside its own terminal model.
//
// This is what makes a session survive a reboot. The log holds the bytes the pty produced, so feeding
// them through the model reconstructs the screen and scrollback as the last client saw them.
//
// Seeding the session's own model rather than producing a one-off blob is the whole point. A revived
// session is an adopted one whose history is on disk instead of in a living shim, so it takes the same
// route: seed the model, hand the graphics store over, and let the ordinary restore path serve every
// client from it. Manager.adopt calls this in the same place it calls replayShimHistory.
//
// The rejected alternative was a blob: replay into a throwaway terminal, serialize it, give the bytes to
// the first client to attach. That made reboot recovery a special case reaching exactly one client, so a
// second client saw a session with no history and `cm read` saw only the new shell. The images were what
// exposed it, since a blob bypasses the transmissions and placements the ordinary attach path emits.
//
// Called before the session exists, which is load-bearing. newSession starts the output pump, and content
// written to the model after that would land after whatever the new shell has already printed, putting the
// pre-reboot screen below the new prompt.
//
// The model is not told the log's sequence positions, and must not be: those numbers belong to a dead
// incarnation, and the new shim numbers from zero. What the model holds is therefore history with no
// position plus, once the pump runs, everything the log does account for. That is exactly the pairing
// attach relies on, because a client receives the serialized screen, which carries the history, and then
// streams from the model's position in the current log.
func seedFromPersistedLog(
	logPath string,
	term Terminal,
	gfx *graphics.Store,
	limits seqlog.FileLimits,
) error {
	if logPath == "" {
		return ErrNothingToRestore
	}
	if _, statErr := os.Stat(logPath); statErr != nil {
		// A missing log is the normal case for a session that never persisted, so it is not an
		// error worth surfacing as one.
		return ErrNothingToRestore
	}
	if term == nil {
		// Without an emulator the bytes cannot become a screen. Replaying them raw would dump an entire
		// log into the terminal, so refusing is better.
		return ErrNothingToRestore
	}

	f, err := seqlog.OpenFile[seq.Shim](logPath, limits)
	if err != nil {
		return fmt.Errorf("opening persisted log: %w", err)
	}
	defer f.Close()

	oldest, end := f.Bounds()
	if end == oldest {
		return ErrNothingToRestore
	}

	data, _, err := f.ReadFrom(oldest)
	if err != nil {
		return fmt.Errorf("reading persisted log: %w", err)
	}
	if len(data) == 0 {
		return ErrNothingToRestore
	}

	// The images the log contains, recorded before the bytes are fed so the store and the screen describe
	// the same thing. Scanned rather than read back from the model afterwards: libghostty stores decoded
	// pixels, and re-encoding was measured at 90x the inbound size. What the log holds is what cm
	// forwarded, so a transfer in it is already inlined and already named and a transmission is byte-exact.
	// replayShimHistory does the same for the same reason.
	if gfx != nil {
		var scanner graphics.Scanner
		for _, seg := range scanner.Scan(data) {
			if !seg.Graphics {
				continue
			}
			resolved, rerr := graphics.ReadTransfer(seg.Cmd)
			if rerr != nil {
				// A transfer naming a file that has gone since the reboot, which is the normal fate of a
				// temp file. Nothing to record: the image is either on the replayed screen or it is not.
				continue
			}
			recordGraphics(gfx, resolved)
		}
	}

	// One write rather than chunked: the emulator's parser is stateful across calls anyway, and a
	// single call avoids any question of a sequence being split at a boundary.
	if err := term.Write(data); err != nil {
		return fmt.Errorf("replaying persisted log: %w", err)
	}

	// Anything the emulator generated in response is discarded. Those are answers to queries the
	// program asked before the reboot, and that program no longer exists; delivering them to a new
	// shell would inject stray input.
	term.TakePending()
	return nil
}
