package client

import (
	"io"
	"log/slog"

	"github.com/chancez/cm/internal/ansi"
)

// screen is the one writer to a client's terminal, and the reason it exists is ordering.
//
// cm has more than one thing to say to a terminal. The session's output is one. The others are cm's
// own: the serialized screen replayed on attach, a query proxied to the terminal on the session's
// behalf, the outage notice, and the window title. Before this they were separate writers with no
// ordering between them, and one of them wrote to os.Stdout without going through TTY at all.
//
// That cost a bug whose symptom was nowhere near its cause. A window title written from OnMetadata
// landed between two halves of a split escape sequence, so the terminal received
// `ESC [ 38:2:232 ESC ] 2;nvim ... BEL :102:113m`, discarded the aborted CSI, and printed
// `:102:113m` as text. The stray characters shifted the line, the line scrolled the screen, and every
// cell nvim did not happen to repaint afterwards held stale content until a ctrl-l. It took three
// rounds of instrumentation to find, because every capture taken inside cm missed the writer that
// bypassed cm's own abstraction: only `kitty --dump-bytes`, the terminal's own record, showed 160
// bytes arriving that cm had not sent through TTY.
//
// The invariant this establishes, and the one worth keeping: **exactly one writer per shared byte
// stream, and bytes cm injects wait for a sequence boundary.** The pty side of cm already worked this
// way, in Session.queueOrWriteReply, which holds a locally generated reply behind any outstanding
// question. The terminal side had five writers and no ordering point at all. See docs/architecture.md.
type screen struct {
	out io.Writer
	log *slog.Logger

	// paint is false when the destination is not a terminal, which is a follower taking bytes
	// programmatically. Nothing cm generates is written then: escape bytes in a pipe corrupt whatever
	// is consuming it, which is the same distinction the gap repaint draws on NoRestore.
	paint bool

	// track follows what the terminal has received, so Inject knows whether a sequence is half-written.
	track ansi.Tracker
	// transcript records what was written and where it came from, for tests. Nil in a released binary,
	// where the type has no fields at all. See transcript.go.
	transcript *Transcript
	// held is what Inject could not write yet, released at the next boundary.
	held []byte
}

// maxHeld bounds what can be waiting for a boundary.
//
// Reached only if the session's output opens a sequence and stays inside it while cm keeps generating
// bytes, which ansi.Tracker already bounds separately. Past this the held bytes are dropped rather
// than written, and dropping is the safe direction: every one of them is cm's own, and none is session
// content. A title is replaced by the next one, a proxied query expires on the server, and the outage
// notice repaints on its timer. Writing them anyway would be the bug this type exists to prevent.
const maxHeld = 4096

// newScreen returns the writer for one attachment.
func newScreen(out io.Writer, paint bool, log *slog.Logger) *screen {
	return &screen{out: out, paint: paint, log: log, transcript: newTranscript()}
}

// screenDest is where an attachment's bytes go: the caller's writer when it supplied one, and the
// terminal otherwise.
//
// One place rather than the same conditional at each write site, which is how the restore blob and the
// session's output came to make the choice separately.
func screenDest(tty *TTY, opts Options) io.Writer {
	if opts.Output != nil {
		return opts.Output
	}
	return tty
}

// injectWriter adapts a screen to io.Writer, for callers that build their bytes with fmt and would
// otherwise need a buffer of their own.
//
// Reports every byte consumed even when the screen dropped them, which is what an io.Writer must do:
// the caller wrote them, and a short count would be read as an error. Same contract as ansi.Stripper,
// which also drops on purpose.
type injectWriter struct{ s *screen }

func (w injectWriter) Write(p []byte) (int, error) {
	if err := w.s.inject(p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// session writes the session's own bytes, verbatim and immediately.
//
// Never withheld, whatever state the stream is in: these bytes are the program's, they are what the
// user is waiting to see, and holding them would trade a rendering fault for a stall.
func (s *screen) session(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	return s.emit(p, "session")
}

// inject writes bytes cm generated itself, at a point where they cannot split a sequence.
//
// Held rather than dropped when the stream is mid-sequence, and released by the next session write
// that completes it. The alternative, writing immediately, is the bug: see the type comment.
func (s *screen) inject(p []byte) error {
	if !s.paint || len(p) == 0 {
		return nil
	}
	if !s.track.InSequence() {
		return s.emit(p, "inject")
	}
	if len(s.held)+len(p) > maxHeld {
		// Logged rather than silent, because a client dropping what it meant to say looks identical to
		// one that never had anything to say.
		s.log.Warn("dropped bytes cm generated for the terminal: the session's output has been "+
			"mid-sequence for too long", "held", len(s.held), "dropped", len(p))
		return nil
	}
	// Logged because the symptom of a hold is silence, and it looks identical to cm having nothing to say.
	// The overlay made that visible: with an idle session there is no next write to release the hold, so
	// the bar simply never appeared and the keys still worked. Fed minus Boundary is how long the partial
	// sequence is, which is what identifies whose it was.
	s.log.Debug("holding bytes cm generated until the session's sequence ends",
		"bytes", len(p), "held", len(s.held), "partial", s.track.Fed()-s.track.Boundary())
	s.held = append(s.held, p...)
	return nil
}

// emit writes to the terminal and keeps the tracker in step with it.
//
// Everything goes through here, injected bytes included, so the tracker's view is what the terminal
// actually received rather than what the session sent. An injection that was itself incomplete would
// otherwise leave the tracker claiming a boundary that does not exist.
func (s *screen) emit(p []byte, kind string) error {
	if _, err := s.out.Write(p); err != nil {
		return err
	}
	// Recorded here rather than where the write was requested, so the record is what the terminal
	// received in the order it received it. Recording at the request instead described intent, and a
	// held injection then appeared before the bytes that released it, which made a transcript of correct
	// behaviour look like a violation and a transcript of the bug look fine.
	s.transcript.record(kind, p)
	s.track.Feed(p)
	// Checked after every write rather than only after a session write, since a released batch can
	// itself end at a boundary that lets the next one go.
	if len(s.held) > 0 && !s.track.InSequence() {
		held := s.held
		s.held = nil
		return s.emit(held, "inject")
	}
	return nil
}
