//go:build cgo

package vt

import (
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/chancez/cm/internal/paths"
)

// recovered turns a panic into an error, so a fault in the terminal model does not take the
// session with it.
//
// The session's real value is the shell and its scrollback, and terminal state is a derived cache.
// Losing the cache costs screen restore on the next attach; losing the process costs the user's
// work. zmx saw this twice, where a malformed sequence reached an unreachable branch in the VT
// library and destroyed the session.
//
// This covers panics on the Go side of the boundary, which includes anything the binding itself
// gets wrong. It cannot catch a segfault inside libghostty: that is a signal, not a panic, and it
// ends the process regardless. Keeping the cgo surface narrow is the defense against that.
func recovered(r any, err error, what string) error {
	if r == nil {
		return err
	}
	// The stack is the only record of where this happened, and it is worth keeping even though the
	// caller only sees an error.
	return fmt.Errorf("%s: recovered from panic: %v\n%s", what, r, debug.Stack())
}

// SessionTerminal adapts a Terminal to what the server needs from a terminal model.
//
// Two things it adds. It serializes access, because the server reads a snapshot from the
// attach path while the output pump writes, and a Terminal is not safe for concurrent use. And
// it queues bytes the emulator generates rather than writing them straight back, because
// libghostty forbids re-entering the terminal from a callback and the natural implementation
// of "send this to the pty" would do exactly that.
type SessionTerminal struct {
	mu   sync.Mutex
	term *Terminal

	// pending holds bytes destined for the pty, produced by callbacks during Write.
	pending [][]byte
	// title and pwd are the most recent values reported by the shell.
	title string
	pwd   string
}

// NewSessionTerminal creates a terminal model for a session.
//
// scrollbackLines bounds retained history; zero means unlimited.
func NewSessionTerminal(rows, cols uint16, scrollbackLines int) (*SessionTerminal, error) {
	st := &SessionTerminal{}

	// Process-wide rather than per terminal, which is what SetXtversion documents. Every session in a
	// server reports the same cm version, so setting it on each construction is idempotent.
	SetXtversion(paths.Name + " " + paths.Version())

	term, err := New(rows, cols, Callbacks{
		// Queued rather than written directly: this fires inside Write, and touching the pty
		// from here would mean re-entering code the emulator has locked.
		//
		// The mode-denial rewrite happens here rather than in the server, because this is where a
		// reply the *model* generated can still be told apart from anything else on the pty. See
		// DenyModes: the emulator implements left/right margins and in-band size reports correctly,
		// and for both of them cm cannot keep the promise its model would make. Reporting the model's
		// own answer scrolled both halves of an nvim vertical split, and stopped nvim resizing when a
		// kitty split closed.
		WritePty: func(data []byte) {
			// Nothing is queued when the rewrite consumed the whole chunk, which happens when the model
			// emitted only an in-band size report. An empty entry would be an entry: TakePending's
			// callers treat one as a reply to deliver, and a zero-length write to the pty is a write
			// that took a queue slot and an ordering position for nothing.
			if out := DenyModes(data); len(out) > 0 {
				st.pending = append(st.pending, out)
			}
		},
		TitleChanged: func(title string) { st.title = title },
		PwdChanged:   func(pwd string) { st.pwd = pwd },
		// Identify cm rather than the emulator it embeds. Only reaches a program when no client is
		// attached, since an attached terminal answers XTVERSION itself, and that is exactly the case
		// where cm is the terminal a program is talking to. Left unset it answers "libghostty", which
		// names a library rather than anything a program can act on.
		ReportXtversion: true,
	})
	if err != nil {
		return nil, err
	}
	if scrollbackLines > 0 {
		if err := term.SetScrollbackLimit(scrollbackLines); err != nil {
			term.Close()
			return nil, err
		}
	}
	st.term = term
	return st, nil
}

// Write feeds terminal output to the emulator.
//
// Callbacks run during this call and only append to fields this method already owns the lock
// for, which is why they need no locking of their own.
func (s *SessionTerminal) Write(p []byte) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "writing to the terminal model") }()
	return s.term.Write(p)
}

// Restore returns bytes reproducing the current screen.
func (s *SessionTerminal) Restore() (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "serializing the terminal model") }()
	return s.term.Restore()
}

// Resize changes the model's size to match the terminal showing it.
func (s *SessionTerminal) Resize(rows, cols uint16) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "resizing the terminal model") }()
	return s.term.Resize(rows, cols)
}

// SizeReport returns the in-band size report owed for a resize to this size, or nil when none is.
//
// Nil when the program has not enabled mode 2048, which is the overwhelmingly common case: sending an
// unrequested report would put bytes on the pty that the program has no reason to expect, and a shell
// would echo them at its prompt.
//
// Combined into one call rather than exposing the mode and the formatter separately, so a caller cannot
// send a report without checking, which is the only way to get this wrong. The size is passed in rather
// than read from the model because the caller knows the size it is setting and the model may not have
// been resized yet; the report has to describe the new size either way.
//
// An error reading the mode is reported as "no report owed" rather than propagated. The caller is the
// resize path, and failing a resize because a capability query failed would turn a cosmetic problem into
// a broken session. The consequence of a wrong nil here is the bug this exists to fix, which is why the
// error is returned as well as swallowed: the caller logs it.
func (s *SessionTerminal) SizeReport(rows, cols uint16) (report []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "reading the in-band resize mode") }()
	on, err := s.term.InBandResizeMode()
	if err != nil || !on {
		return nil, err
	}
	return SizeReport(rows, cols), nil
}

// TakePending returns and clears bytes the emulator generated for the pty.
//
// The caller must deliver these, since programs that query the terminal block until answered.
func (s *SessionTerminal) TakePending() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	out := s.pending
	s.pending = nil
	return out
}

// Title returns the last title the shell reported.
func (s *SessionTerminal) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// Pwd returns the last directory the shell reported, as emitted, so usually a file:// URI.
func (s *SessionTerminal) Pwd() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pwd
}

// Plain returns the terminal contents as plain text.
func (s *SessionTerminal) Plain() (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "rendering terminal contents") }()
	return s.term.Plain()
}

// Tail returns the last lines of the terminal contents as plain text.
func (s *SessionTerminal) Tail(lines int, unwrap bool) (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "rendering recent terminal contents") }()
	return s.term.Tail(lines, unwrap)
}

// TailVT returns the last lines of the terminal contents as escape sequences.
func (s *SessionTerminal) TailVT(lines int) (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "rendering recent terminal contents as escapes") }()
	return s.term.TailVT(lines)
}

// VT returns the terminal contents as escape sequences.
func (s *SessionTerminal) VT() (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "rendering terminal contents") }()
	return s.term.VT()
}

// HTML returns the terminal contents as HTML.
func (s *SessionTerminal) HTML() (out []byte, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() { err = recovered(recover(), err, "rendering terminal contents") }()
	return s.term.HTML()
}

// Close releases the emulator.
func (s *SessionTerminal) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term.Close()
}

// FocusReporting reports whether the program in the session asked to be told about focus changes
// (DECSET 1004).
//
// Worth knowing because such a program uses focus to decide whether anyone is watching: a TUI may
// skip rendering, and something long-running may raise a desktop notification only when
// unobserved. Detaching is exactly "nobody is watching", so it should be reported as focus loss.
func (s *SessionTerminal) FocusReporting() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	on, err := s.term.focusEventMode()
	if err != nil {
		return false
	}
	return on
}

// KittyKeyboardProtocol reports whether a program in the session has the kitty keyboard protocol
// enabled, meaning its flags are anything other than zero.
//
// Used to recognize a keyboard event no program asked for. A terminal encodes keys in this protocol only
// while some program has pushed flags, so an event in that encoding arriving when the model's flags are
// zero was generated for a program that has since exited. See input.IsStaleEvent.
//
// The model rather than a separate tracker, for the reason InBandResizeMode gives: the model already
// parses every byte the program writes, and it cannot lag the real terminal here, because a program's
// push reaches the model on its way to the client rather than after it.
func (s *SessionTerminal) KittyKeyboardProtocol() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	flags, err := s.term.kittyKeyboardFlags()
	if err != nil {
		// Treated as on, so an unreadable model never causes an event to be dropped.
		return true
	}
	return flags != 0
}
