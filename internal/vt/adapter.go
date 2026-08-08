package vt

import (
	"fmt"
	"runtime/debug"
	"sync"
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

	term, err := New(rows, cols, Callbacks{
		// Queued rather than written directly: this fires inside Write, and touching the pty
		// from here would mean re-entering code the emulator has locked.
		WritePty: func(data []byte) {
			st.pending = append(st.pending, data)
		},
		TitleChanged: func(title string) { st.title = title },
		PwdChanged:   func(pwd string) { st.pwd = pwd },
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
