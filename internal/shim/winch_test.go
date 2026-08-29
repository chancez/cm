package shim

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// A same-size resize must still make the shell see a window-size change.
//
// The kernel raises SIGWINCH only when the size actually differs, so a client reattaching at the
// size the session already has gets nothing, and a program that repaints only on SIGWINCH keeps
// drawing against a screen that is now a replayed snapshot. Confirmed to be kernel behavior on a
// bare pty with no cm involved.
//
// The trap uses short sleeps because bash defers a trap until the running command finishes, so a
// long sleep would swallow the signal and look like a missing SIGWINCH.
func TestResizeSignalFiresAtSameSize(t *testing.T) {
	sess, err := Start(Config{
		Session: "winch",
		Command: []string{"/bin/sh", "-c", `
			trap 'echo GOT_WINCH' WINCH
			echo READY
			i=0
			while [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done
		`},
		Rows: 24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { sess.Signal(9, true) })

	r := sess.Log().Subscribe(0)
	defer r.Close()
	readUntil(t, r, "READY")

	// Control: a real size change must fire, proving this test can observe a SIGWINCH at all.
	if err := sess.Resize(40, 100, 0, 0); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if !waitFor(t, r, "GOT_WINCH") {
		t.Fatal("control failed: a real size change produced no SIGWINCH, so this test cannot detect one")
	}

	// The case under test: the same size again.
	if err := sess.ResizeSignal(40, 100, 0, 0); err != nil {
		t.Fatalf("ResizeSignal() error = %v", err)
	}
	if !waitFor(t, r, "GOT_WINCH") {
		t.Error("ResizeSignal at an unchanged size delivered no SIGWINCH; a TUI would not repaint")
	}

	// And the size must be left exactly as asked, not one row short from the nudge.
	rows, cols, err := sess.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if rows != 40 || cols != 100 {
		t.Errorf("Size() after ResizeSignal = (%d, %d), want (40, 100)", rows, cols)
	}
}

// A plain Resize must not nudge, since the kernel already signals when the size differs.
func TestResizeDoesNotNudgeOnRealChange(t *testing.T) {
	sess, err := Start(Config{
		Session: "nonudge",
		Command: []string{"/bin/sh", "-c", "echo READY; i=0; while [ $i -lt 600 ]; do sleep 0.05; i=$((i+1)); done"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { sess.Signal(9, true) })

	r := sess.Log().Subscribe(0)
	defer r.Close()
	readUntil(t, r, "READY")

	if err := sess.ResizeSignal(50, 120, 0, 0); err != nil {
		t.Fatalf("ResizeSignal() error = %v", err)
	}
	rows, cols, err := sess.Size()
	if err != nil {
		t.Fatalf("Size() error = %v", err)
	}
	if rows != 50 || cols != 120 {
		t.Errorf("Size() = (%d, %d), want (50, 120)", rows, cols)
	}
}

// waitFor reports whether want appears before a short deadline, without failing the test, so a
// caller can distinguish a control failure from the behavior under test.
func waitFor(t *testing.T, r *seqlog.Reader[seq.Shim], want string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var sb strings.Builder
	for {
		c, err := r.Next(ctx)
		if err != nil {
			return false
		}
		sb.Write(c.Data)
		if strings.Contains(sb.String(), want) {
			return true
		}
	}
}
