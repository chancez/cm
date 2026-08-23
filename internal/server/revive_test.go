package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// Attaching by ID to a session whose shell has gone must revive it, not refuse.
//
// It used to refuse, because reviving allocated a new ID and so could not return the one asked for. That
// made an ID a handle that stopped working the moment its shell exited, which is the opposite of what an
// identity is for, and left a session with no names unrevivable by anything.
//
// Asserted through the store because a unit test cannot spawn a shim: what matters is that Open took the
// revive path at all, which the deleted record shows, and that it did not fail with the old refusal.
func TestOpenByIDRevivesAnEndedSession(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		ID:         "aaaa2222",
		ShimSocket: dirs.ShimSocket("aaaa2222"),
		LogPath:    dirs.SessionLog("aaaa2222"),
		State:      store.StateExited,
		ExitCode:   3,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	// Spawning a real shim needs the cm binary, so creation fails after the decision under test.
	mgr.selfExe = "/nonexistent/cm"
	_, _, err := mgr.Open(ctx, OpenOptions{
		Ref: paths.FormatSessionID("aaaa2222"), Rows: 24, Cols: 80,
	})
	if err == nil {
		t.Fatal("Open() with a bogus shim binary succeeded, want failure")
	}
	// The old behavior, named exactly so this test fails if it comes back: it reported the state rather
	// than starting anything.
	if strings.Contains(err.Error(), "is exited") {
		t.Errorf("Open() by ID error = %v, want it to have tried to revive the session", err)
	}
	// The shim it tried to spawn had to be this session's, which is what says the revive kept the ID
	// rather than allocating another. Read off the error because a unit test cannot spawn one, and the
	// failure names the session it was spawning for.
	if !strings.Contains(err.Error(), "aaaa2222") {
		t.Errorf("Open() by ID error = %v, want it to name aaaa2222: the revive must keep the ID", err)
	}
	if _, err := st.Get(ctx, "aaaa2222"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Get() error = %v, want the record replaced rather than left alone", err)
	}
}

// An ID with no record behind it is stale, and must not create a session. An ID is issued by cm rather
// than chosen, so one cm has never issued came from a database that is gone, and inventing a session for it
// would put a caller somewhere it did not ask to be.
func TestOpenByIDDoesNotCreate(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	_, _, err := mgr.Open(ctx, OpenOptions{
		Ref: paths.FormatSessionID("nothere2"), Rows: 24, Cols: 80,
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Open() by an unknown ID error = %v, want ErrNotFound", err)
	}
	records, err := st.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(records) != 0 {
		t.Errorf("List() = %+v, want no session created for a stale ID", records)
	}
}

// Reviving by name keeps the ID, so the name needs no rebinding and keeps pointing at the same session.
//
// The binding is the assertion: a revive that allocated a new ID would have had to move the name, and a
// revive that moved nothing while allocating one would leave the name pointing at the session that just
// went away.
func TestReviveByNameKeepsTheIDAndTheBinding(t *testing.T) {
	mgr, st, dirs := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{
		ID:         "aaaa2222",
		ShimSocket: dirs.ShimSocket("aaaa2222"),
		LogPath:    dirs.SessionLog("aaaa2222"),
		State:      store.StateExited,
	}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := st.Bind(ctx, store.Binding{
		Name: "work", SessionID: "aaaa2222", OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	mgr.selfExe = "/nonexistent/cm"
	_, _, err := mgr.Open(ctx, OpenOptions{Ref: "work", Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("Open() with a bogus shim binary succeeded, want failure")
	}
	// Same reasoning as the by-ID case: the ID it tried to build a shim for is the one it kept.
	if !strings.Contains(err.Error(), "aaaa2222") {
		t.Errorf("Open() error = %v, want it to name aaaa2222: a revive keeps the ID", err)
	}

	binding, err := st.Binding(ctx, "work")
	if err != nil {
		t.Fatalf("Binding() after the revive error = %v", err)
	}
	want := store.Binding{
		Name:      "work",
		SessionID: "aaaa2222",
		OnKill:    store.KillTarget,
		CreatedAt: binding.CreatedAt,
	}
	if binding != want {
		t.Errorf("Binding() = %+v\nwant %+v", binding, want)
	}
}

// A name pointing at a record that is gone heals rather than failing forever.
//
// Nothing removes a record without its names, so this is a state that should not arise. Refusing would
// leave the name permanently unusable while reading as though the session were merely missing, which is
// worse than recovering and saying so in the log.
func TestNameWithNoRecordCreatesAndRebinds(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	if err := st.Bind(ctx, store.Binding{
		Name: "orphaned", SessionID: "gone2222", OnKill: store.KillTarget,
	}); err != nil {
		t.Fatalf("Bind() error = %v", err)
	}

	mgr.selfExe = "/nonexistent/cm"
	_, _, err := mgr.Open(ctx, OpenOptions{Ref: "orphaned", Rows: 24, Cols: 80})
	if err == nil {
		t.Fatal("Open() with a bogus shim binary succeeded, want failure")
	}
	// Named as a shim failure rather than as a missing session: the point is that it went on to create
	// one rather than reporting the dangling name.
	if errors.Is(err, store.ErrNotFound) {
		t.Errorf("Open() error = %v, want it to have created a session for the dangling name", err)
	}
}
