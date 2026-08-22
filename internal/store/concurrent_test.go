package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// The store is reached from many goroutines at once: every RPC handler touches it, the expiry sweep runs on a
// ticker, and each session's watcher records its outcome independently. sqlite allows one writer at a time, so
// this is the layer where contention either is handled or surfaces as SQLITE_BUSY in whichever caller was
// unlucky.
//
// Open uses a single connection deliberately, which serializes access and avoids that entirely. These tests pin
// that: the point is not that sqlite is fast but that no caller ever sees a spurious failure.

// concurrentOps is how many goroutines each test runs.
const concurrentOps = 40

// Concurrent creates all succeed, and every record is readable afterwards.
func TestConcurrentCreates(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	errs := make(chan error, concurrentOps)
	for i := range concurrentOps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := st.Create(ctx, Session{ID: fmt.Sprintf("s%d", i)}); err != nil {
				errs <- fmt.Errorf("Create(s%d): %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("%v", err)
	}

	got, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != concurrentOps {
		t.Errorf("List() returned %d records, want %d", len(got), concurrentOps)
	}
}

// Two creates of the same ID: exactly one wins.
//
// The name is the identity of a session, and it is what a socket path is built from. Two sessions sharing one
// would mean two shims on one socket, so this has to be decided by the database rather than by a check-then-act
// in the caller, which is a race by construction.
func TestConcurrentCreatesOfTheSameIDConflict(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		succeeded int
	)
	for range concurrentOps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Create(ctx, Session{ID: "contested"}); err == nil {
				mu.Lock()
				succeeded++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if succeeded != 1 {
		t.Errorf("%d creates of the same ID succeeded, want exactly 1", succeeded)
	}
}

// Concurrent readers and writers do not produce SQLITE_BUSY.
//
// This is the claim the single connection exists to make good on. A busy error surfacing here would appear to a
// user as a random command failing while something else happened to be writing.
func TestConcurrentReadersAndWriters(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	// Something to read.
	for i := range 10 {
		if err := st.Create(ctx, Session{ID: fmt.Sprintf("base%d", i)}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
	}

	var wg sync.WaitGroup
	errs := make(chan error, concurrentOps*3)

	for i := range concurrentOps {
		wg.Add(3)

		// Writers, as a session being created or finishing.
		go func(i int) {
			defer wg.Done()
			if err := st.Create(ctx, Session{ID: fmt.Sprintf("new%d", i)}); err != nil {
				errs <- fmt.Errorf("Create: %w", err)
			}
		}(i)

		// Updaters, as a watcher recording an outcome.
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("base%d", i%10)
			if err := st.Apply(ctx, name, Update{State: new(StateExited)}); err != nil &&
				!errors.Is(err, ErrNotFound) {
				errs <- fmt.Errorf("Apply: %w", err)
			}
		}(i)

		// Readers, as `cm list` and completion.
		go func() {
			defer wg.Done()
			if _, err := st.List(ctx); err != nil {
				errs <- fmt.Errorf("List: %w", err)
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		// Any error here is the failure: contention is meant to be invisible to callers.
		t.Errorf("concurrent access failed: %v", err)
	}
}

// Concurrent deletes of one record all succeed, and the record ends up gone.
//
// Delete is deliberately idempotent: several callers in the manager delete as cleanup after something else
// already failed, where "it was already gone" is the outcome they want, not an error to handle.
//
// That means the store is not where "no such session" is decided. Manager.Kill reads the record first and
// returns ErrNotFound from there, which is why `cm kill nosuch` exits 1 rather than reporting success. Asserted
// in both places so the split is deliberate rather than accidental (see
// TestKillMissingSessionReportsNotFound in internal/server): an earlier version of this test expected exactly
// one delete to succeed, which is the contract of a different design.
func TestConcurrentDeletesAreIdempotent(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	if err := st.Create(ctx, Session{ID: "doomed"}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, concurrentOps)
	for range concurrentOps {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := st.Delete(ctx, "doomed"); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("Delete() error = %v, want nil: deleting a gone record is not an error", err)
	}

	if _, err := st.Get(ctx, "doomed"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() after delete error = %v, want ErrNotFound", err)
	}
	// And a delete of a name that never existed is equally fine, which is the property the manager relies on.
	if err := st.Delete(ctx, "never-existed"); err != nil {
		t.Errorf("Delete() of an unknown name error = %v, want nil", err)
	}
}
