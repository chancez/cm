package main

import (
	"os"
	"testing"

	"github.com/creack/pty"

	"github.com/chancez/cm/internal/paths"
)

// Both conditions for calling an attach nested have to hold, and either one alone gets it wrong.
//
// This decides whether the server freezes a session's metadata, so a false positive is the expensive
// direction: it stops a session reporting its own directory and title for as long as the attachment
// lasts, which is worse than the bug being fixed. A false negative only restores the old behavior for
// one attachment.
//
// A real pty rather than a stand-in for the terminal case. The condition *is* "is this file descriptor a
// terminal", so substituting something that merely reports that it is would test the substitute.
func TestInsideCmSession(t *testing.T) {
	// withStdout swaps os.Stdout for the duration, since insideCmSession asks about the real one.
	withStdout := func(t *testing.T, f *os.File, fn func()) {
		t.Helper()
		saved := os.Stdout
		os.Stdout = f
		defer func() { os.Stdout = saved }()
		fn()
	}

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()

	// A pipe, which is what `cm attach x | cat` or a redirect to a file gives.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	defer r.Close()
	defer w.Close()

	t.Run("in a session with a terminal on stdout", func(t *testing.T) {
		// The nested attach: CM_SESSION names the session whose pty this stdout is.
		//
		// Spelled as cm really exports it, which is the ID with its sigil. A bare name here hid a
		// server-side bug: the value is a *reference*, the server looked it up as an ID, and nesting
		// silently never engaged. See Service.hostingParent.
		t.Setenv(paths.SessionEnv(), "@a7k2m9x4")
		withStdout(t, tty, func() {
			if got := insideCmSession(); got != "@a7k2m9x4" {
				t.Errorf("insideCmSession() = %q, want %q: a nested attach was not recognized, so "+
					"the parent goes on recording the child's reports as its own", got, "@a7k2m9x4")
			}
		})
	})

	t.Run("what it declares is a session reference", func(t *testing.T) {
		// The producer half of the contract. The server resolves this value as a reference, and it went
		// wrong at exactly this seam: the client sent "@<id>" and the server looked it up as a bare ID,
		// so nesting never engaged and nothing failed. Asserting the shape here means a change on either
		// side has to be deliberate.
		//
		// Both spellings, because both really occur: a current shim exports the ID with its sigil, and a
		// session created by an older server exported a name.
		for _, value := range []string{paths.FormatSessionID("a7k2m9x4"), "work"} {
			t.Setenv(paths.SessionEnv(), value)
			withStdout(t, tty, func() {
				got := insideCmSession()
				if got != value {
					t.Fatalf("insideCmSession() = %q, want %q verbatim", got, value)
				}
				if err := paths.ValidateSessionRef(got); err != nil {
					t.Errorf("ValidateSessionRef(%q) = %v, want nil: the server resolves this as a "+
						"reference, so anything it cannot resolve silences nesting without failing", got, err)
				}
			})
		}
	})

	t.Run("outside a session", func(t *testing.T) {
		// The overwhelmingly common case: an attach from a real terminal. There is no cm session on
		// the other side and nothing to suspend.
		t.Setenv(paths.SessionEnv(), "")
		withStdout(t, tty, func() {
			if got := insideCmSession(); got != "" {
				t.Errorf("insideCmSession() = %q, want empty: an attach from a real terminal is not "+
					"nested", got)
			}
		})
	})

	t.Run("in a session but stdout is not a terminal", func(t *testing.T) {
		// CM_SESSION is exported into a session's shell and inherited by everything that shell starts,
		// including processes whose output goes somewhere else entirely. `cm attach x > file` from
		// inside a session writes nothing to the parent's pty, so freezing the parent would silence a
		// session that is reporting its own state perfectly well.
		t.Setenv(paths.SessionEnv(), "parent")
		withStdout(t, w, func() {
			if got := insideCmSession(); got != "" {
				t.Errorf("insideCmSession() = %q, want empty: output is a pipe, so nothing this "+
					"process writes reaches the parent's terminal and the parent must keep "+
					"reporting", got)
			}
		})
	})
}
