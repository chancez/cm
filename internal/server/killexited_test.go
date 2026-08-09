package server

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
)

// Killing a session whose shell has just exited must succeed.
//
// Found by a flaky e2e failure under -race: `cm kill --all` after several detached `cm run` invocations
// reported `d5: stopping d5: ttrpc: closed`. The race detector slows everything enough to widen a window that
// is otherwise hard to hit, which is why it showed up there and not in 25 rounds of the same thing without it.
//
// The window: the shell has exited and the shim is shutting down, so its connection is closed, but the session
// is still in the manager's registry. Kill takes the live path, the Shutdown RPC fails with a transport error,
// and without force that is reported as a failure. Meanwhile the thing the caller asked for -- that the session
// be gone -- has already happened.
//
// It matters beyond the noise. `cm kill --all` is what the e2e harness uses for cleanup, and a spurious failure
// there was previously ignored, which is how 437 stray ptys accumulated. It is also just wrong for a user: the
// session is gone and the command says it could not stop it.
func TestKillSucceedsWhenTheShimIsAlreadyGone(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	// A session that ends immediately, so the shim is on its way out from the start.
	rec := startShimInRuntimeDir(t, dirs, "quick", "exit 0")
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("quick")
	if !ok {
		t.Fatal("session was not adopted, so this test would assert nothing")
	}

	// Wait for the shell to have exited, which is what closes the shim's connection. This is the state the
	// race lands in; waiting for it makes the test deterministic rather than timing-dependent.
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not finish within 10s")
	}

	// Still tracked, or the live path this is about would not be taken.
	if _, stillLive := mgr.Get("quick"); !stillLive {
		t.Skip("the session left the registry before the kill, so the window under test was not reached")
	}

	// Without force, which is what `cm kill` and `cm kill --all` send by default.
	if err := mgr.Kill(ctx, "quick", false, 0); err != nil {
		t.Errorf("Kill() error = %v, want nil: the shell has already exited, so the caller's intent is met",
			err)
	}

	// And the record is gone, which is the part that actually matters. The early return this fixes skipped the
	// delete, so the row survived forever: a worse consequence than the noisy error that revealed it.
	if _, err := st.Get(ctx, "quick"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() after Kill error = %v, want ErrNotFound: the record must be removed", err)
	}
}
