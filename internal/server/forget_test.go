package server

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/store"
)

// Forgetting an ended session drops all of it: the row, the names that reach it, and its saved output.
func TestForgetDropsAnEndedSessionEntirely(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	logPath := dirs.SessionLog("finished")
	recordSession(t, st, store.Session{
		ID:               "finished",
		ShimSocket:       dirs.ShimSocket("finished"),
		LogPath:          logPath,
		PersistRequested: true,
		State:            store.StateExited,
	})
	if err := os.WriteFile(logPath, []byte("what it printed\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	if err := mgr.Forget(ctx, "finished"); err != nil {
		t.Fatalf("Forget() error = %v", err)
	}

	if _, err := st.Get(ctx, "finished"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
	if _, err := os.Stat(logPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Stat(%s) error = %v, want the saved output removed", logPath, err)
	}
	if _, err := mgr.Resolve(ctx, "finished"); err == nil {
		t.Error("Resolve() succeeded, want the names released")
	}
}

// A session that is still running is refused, and this is the property that makes Forget a separate call
// from Kill: forgetting a live session would leave a shim holding a pty that nothing can name again.
//
// Both ways a session can be alive are covered, because they are checked separately: one this server is
// proxying, and one recorded as running with no entry in the registry, which is what a session belonging to
// another server or outliving a restart looks like.
func TestForgetRefusesALiveSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		adopted bool
	}{
		{name: "in the registry", adopted: true},
		{name: "recorded as running only", adopted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, st, dirs := newTestManager(t, nil)
			ctx := context.Background()

			rec := startShimInRuntimeDir(t, dirs, "alive", "sleep 30")
			recordSession(t, st, rec)
			if tc.adopted {
				if err := mgr.Reconcile(ctx); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
				if _, live := mgr.Get("alive"); !live {
					t.Fatal("session was not adopted, so this case is the other one")
				}
			} else if _, live := mgr.Get("alive"); live {
				t.Fatal("session is in the registry, so this case is the other one")
			}

			err := mgr.Forget(ctx, "alive")
			if err == nil {
				t.Fatal("Forget() = nil error, want a refusal: the shell is still running")
			}
			if !strings.Contains(err.Error(), "still running") {
				t.Errorf("error = %q, want it to say the session is still running", err)
			}
			// The refusal has to be complete. A partial forget that released the names or removed the log
			// would leave a live session nobody can reach, which is worse than either outcome.
			if _, err := st.Get(ctx, "alive"); err != nil {
				t.Errorf("Get() error = %v, want the record kept", err)
			}
			if _, err := mgr.Resolve(ctx, "alive"); err != nil {
				t.Errorf("Resolve() error = %v, want the names kept", err)
			}
		})
	}
}
