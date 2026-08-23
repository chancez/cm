package server

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/chancez/cm/internal/store"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// goneShellShim is a shim whose Shutdown fails the way one does when the shell has already exited.
//
// The shim signals the shell's process group to stop it, and if the shell went in the window before the
// shim reaped it, that call fails with ESRCH rather than with either sentinel the server knew about.
type goneShellShim struct{ *flakyStateShim }

func (g *goneShellShim) Shutdown(
	context.Context, *shimv1.ShutdownRequest,
) (*shimv1.ShutdownResponse, error) {
	return nil, syscall.ESRCH
}

// A kill must not report failure for a session it has disposed of.
//
// The process being gone is what the caller asked for. It used to surface as `stopping <id>: no such
// process` with the record deleted anyway, so the session was gone and the command said it had failed.
// Reached most often by `cm rebind --replace`, which ends a session created moments earlier.
func TestKillTreatsAGoneShellAsDone(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	socket := serveFakeShim(t, &goneShellShim{flakyStateShim: &flakyStateShim{pid: 4242}})
	rec := store.Session{ID: "aaaa2222", ShimSocket: socket, State: store.StateRunning}
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	sess, err := newSession(rec, nil, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	// Closed by the test rather than by Kill, which shuts the session's shim down but does not close the
	// server's side of it. Without this the fake's Subscribe handler stays blocked and the ttrpc server
	// cannot finish shutting down, which hangs the test rather than failing it.
	t.Cleanup(sess.Close)
	mgr.mu.Lock()
	mgr.sessions[rec.ID] = sess
	mgr.mu.Unlock()

	if _, err := mgr.Kill(ctx, "aaaa2222", false, 0); err != nil {
		t.Errorf("Kill() error = %v, want nil: the shell being gone is the requested outcome", err)
	}
	// And the record goes, which is what made the old error a lie: it reported a failure for something
	// it had completed.
	if _, err := st.Get(ctx, "aaaa2222"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}
