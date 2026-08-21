package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/chancez/cm/internal/store"
)

// A kill must be attributable afterwards, because a lost session is diagnosed from logs alone.
//
// This encodes a real dead end. A session holding hours of work died, and the only trace anywhere was
// the shim's "shim exiting" with exit_code=-1. That says the shell was terminated by *some* signal and
// nothing else: not which signal, not whether cm did it. `cm kill`, `cm doctor --repair`, and a `kill`
// typed outside cm all produced identical evidence, and Manager.Kill logged nothing whatsoever while
// also deleting the session record. So the question "did cm kill my session, and why" was unanswerable.
//
// The signal is asserted as well as the name, since "cm killed it" without which signal does not
// distinguish an ordinary `cm kill` from a `--force` SIGKILL, and that difference is the difference
// between a shell asked to leave and one that was not.
func TestKillIsAttributableInTheLog(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("attributable", "sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Locked, because the manager logs from more than one goroutine. Kill logs on this one while the
	// session's watch goroutine logs "session ended" as the shell dies, so a bare bytes.Buffer is a real
	// data race: caught by -race failing about one run in four, and the same reason internal/client has
	// a lockedBuffer.
	var logged lockedBuffer
	mgr.SetLogger(slog.New(slog.NewTextHandler(&logged, nil)))

	if _, err := mgr.Kill(ctx, "attributable", false, 9); err != nil {
		t.Fatalf("Kill() error = %v", err)
	}

	got := logged.String()
	// Each of these answers one of the questions that had no answer: was it cm, which session, and what
	// was sent. Checked as substrings of the structured output rather than by parsing, matching how the
	// other log assertions here work.
	for _, want := range []string{
		"killing session",
		`session=attributable`,
		`signal=9`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("kill log is missing %q, so a lost session cannot be attributed to cm.\ngot:\n%s",
				want, got)
		}
	}
}

// lockedBuffer is a bytes.Buffer safe for a logger written from more than one goroutine.
//
// The manager logs from the caller's goroutine and from each session's watch goroutine, so a test that
// asserts on its output has two writers and a reader. Mirrors the one in internal/client, which exists
// for the same reason; kept per package rather than shared, since a test helper exported between
// packages would have to live in non-test code.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
