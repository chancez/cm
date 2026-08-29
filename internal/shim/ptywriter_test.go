package shim

import (
	"bytes"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// TestConcurrentPtyWritesDoNotInterleave pins the assumption that lets cm have several writers on the
// pty.
//
// docs/architecture.md names two shared byte streams, the pty and each client's terminal, and requires
// exactly one writer per stream. The terminal half is enforced in code, by internal/client.screen, after
// a window title landed inside a program's SGR and left stale cells all over nvim. The pty half is not,
// and has several writers by construction: client typing, a client's answer to a query cm proxied, cm's
// own emulator replies, and the in-band resize reports, each arriving at Session.Write on its own
// goroutine. Session.Write calls ptmx.Write outside the lock.
//
// That looks like the same bug, and it is not, because the tty layer serializes a write to a pty master
// for its whole duration. Measured here rather than assumed: 262148 bytes written concurrently with 4000
// short writes, three runs, and not one short write landed inside the payload's span. The window was
// demonstrably open, with short writes recorded both before offset 2007 and after offset 18395 in the
// echoed stream, so the payload's integrity is the tty's doing rather than luck of scheduling.
//
// So the reason there is no lock is a real one, and this test is what keeps it a reason rather than an
// assumption. It fails if cm ever routes pty writes through a buffer of its own, or on a platform whose
// tty layer does not serialize, and either of those is the point at which the pty side needs what
// internal/client.screen does for the terminal side.
func TestConcurrentPtyWritesDoNotInterleave(t *testing.T) {
	// Raw mode with echo off, so the shell passes input through to output verbatim. Canonical mode would
	// line-buffer and do erase processing on a binary payload, which would mangle the fixture and prove
	// nothing about cm.
	s, err := Start(Config{
		Session: "ptywriter",
		Command: []string{"/bin/sh", "-c", "stty raw -echo; cat"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	// A leaked shim holds a pty and macOS caps them at 511 system-wide, so the shell is killed rather
	// than left to its own devices.
	defer s.Signal(syscall.SIGKILL, true)

	r := s.Log().Subscribe(0)
	defer r.Close()

	// A marker first, so the racing writes cannot start while the tty is still in canonical mode.
	if _, err := s.Write([]byte("READY\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	readUntil(t, r, "READY")

	// Shaped like the clipboard reply that motivated the question, and far larger: an OSC 52 response was
	// measured at 18008 bytes, and this is 262148, well past any tty buffer, so the write cannot complete
	// in one syscall and the serialization being tested is the tty's rather than the kernel accepting it
	// all at once.
	big := []byte("\x1b]52;c;" + strings.Repeat("QUJDRA", 262144/6) + "\x07")

	// The other writer, shaped like an in-band resize report: short, cm's own, and emitted whenever a
	// window changes size.
	small := []byte("\x1b[48;24;80t")
	const smallWrites = 4000

	var wg sync.WaitGroup
	wg.Add(2)
	var bigN int
	go func() {
		defer wg.Done()
		var err error
		if bigN, err = s.Write(big); err != nil {
			t.Errorf("writing the large reply: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		// Many, so they overlap the large write's syscall loop rather than having to hit a single window.
		for range smallWrites {
			if _, err := s.Write(small); err != nil {
				t.Errorf("writing the resize report: %v", err)
				return
			}
		}
	}()
	wg.Wait()

	// A short write would be its own defect, and a silent one: Session.Write on the server side discards
	// the WriteResponse, so a truncated payload would reach the program with no error anywhere.
	if bigN != len(big) {
		t.Errorf("Write() wrote %d of %d bytes, and the count is discarded upstream, so a truncated "+
			"reply would reach the program silently", bigN, len(big))
	}

	// Everything the shell echoed, which is what the program inside the session received.
	want := len(big) + len(small)*smallWrites
	var got []byte
	for len(got) < want {
		c, err := r.Next(t.Context())
		if err != nil {
			t.Fatalf("reading the echoed input: %v (got %d of %d bytes)", err, len(got), want)
		}
		got = append(got, c.Data...)
	}

	// The control, and the reason this is not vacuous. Both writers have to be present in the echoed
	// stream, or "the payload is contiguous" is trivially true of a stream nothing raced.
	if seen := bytes.Count(got, small); seen < smallWrites/2 {
		t.Fatalf("only %d of %d short writes reached the program, so the payload was not written "+
			"against real concurrent traffic and its integrity proves nothing", seen, smallWrites)
	}

	// The assertion: the large reply reached the program in one piece. A short write landing inside it
	// aborts the OSC partway through, and the rest of the base64 prints as text, which is the pty-side
	// form of the bug this branch fixed on the terminal side.
	if !bytes.Contains(got, big) {
		detail := "the payload never arrived at all"
		if start := bytes.Index(got, big[:32]); start >= 0 {
			detail = "it starts correctly and is then interrupted"
			if cut := bytes.Index(got[start+1:], small); cut >= 0 {
				detail = "a short write landed inside it"
			}
		}
		t.Errorf("a %d-byte reply written concurrently with %d short writes did not reach the program "+
			"intact: %s.\nThe tty layer no longer serializes writes to a pty master, so the pty needs an "+
			"ordering point of its own, the way internal/client.screen is one for the terminal.",
			len(big), smallWrites, detail)
	}
}
