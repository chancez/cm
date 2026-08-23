package server

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// flakyStateShim answers State with an error the first n times, then reports a pid.
//
// Stands in for the real failure: one RPC to a shim that has just started, under enough load to lose it.
type flakyStateShim struct {
	failures atomic.Int32
	pid      int32
}

func (f *flakyStateShim) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	if f.failures.Add(-1) >= 0 {
		return nil, errors.New("ttrpc: closed")
	}
	return &shimv1.StateResponse{ShellPid: f.pid}, nil
}

// Subscribe blocks, which is what a shim whose shell has produced nothing does.
//
// Returning immediately ends the session, since the pump treats a finished subscription as the shell being
// gone, and a session that is Done cancels the retry under test. That is what this test did first, and it
// failed looking exactly like the bug it was written for.
func (f *flakyStateShim) Subscribe(
	ctx context.Context, _ *shimv1.SubscribeRequest, _ shimv1.Shim_SubscribeServer,
) error {
	<-ctx.Done()
	return ctx.Err()
}

func (f *flakyStateShim) Write(context.Context, *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	return nil, errors.New("unused")
}

func (f *flakyStateShim) Resize(context.Context, *shimv1.ResizeRequest) (*shimv1.ResizeResponse, error) {
	return nil, errors.New("unused")
}

func (f *flakyStateShim) Signal(context.Context, *shimv1.SignalRequest) (*shimv1.SignalResponse, error) {
	return nil, errors.New("unused")
}

func (f *flakyStateShim) Shutdown(
	context.Context, *shimv1.ShutdownRequest,
) (*shimv1.ShutdownResponse, error) {
	return nil, errors.New("unused")
}

// A shell pid that could not be read at creation must still reach the record.
//
// It used to be written once, best effort, and never again: nothing else in the server writes the field and
// `cm list` reads it straight from the record, so one lost RPC meant a healthy session reported pid 0 for
// the rest of its life. That surfaced as an e2e test waiting for a live pid and timing out with no other
// symptom, under two suites running at once.
func TestShellPIDIsRecordedAfterAFailedFirstRead(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	fake := &flakyStateShim{pid: 4242}
	// Fails the first read, which is the one create makes inline, and answers the retry.
	fake.failures.Store(1)
	socket := serveFakeShim(t, fake)

	rec := store.Session{ID: "aaaa2222", ShimSocket: socket, State: store.StateRunning}
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	sess, err := newSession(rec, nil, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(sess.Close)

	// The inline attempt fails, exactly as it does in create.
	if m := mgr.recordShellPID(ctx, sess); m {
		t.Fatal("recordShellPID() = true on a shim whose State fails, want false")
	}
	stored, err := st.Get(ctx, "aaaa2222")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if stored.ShellPID != 0 {
		t.Fatalf("ShellPID = %d before the retry, want 0", stored.ShellPID)
	}

	// The retry is what has to land it.
	go mgr.retryShellPID(sess)

	deadline := time.Now().Add(10 * time.Second)
	for {
		stored, err = st.Get(ctx, "aaaa2222")
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if stored.ShellPID == 4242 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("ShellPID = %d, want 4242: the retry never recorded it", stored.ShellPID)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A session whose record has gone stops the retry rather than running it out.
//
// Otherwise every short-lived session whose first read failed would keep a goroutine retrying against a
// record nothing will ever accept, and log a warning about pid 0 for a session that no longer exists.
func TestShellPIDRetryStopsWhenTheRecordIsGone(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	fake := &flakyStateShim{pid: 4242}
	socket := serveFakeShim(t, fake)
	sess, err := newSession(store.Session{
		ID: "bbbb3333", ShimSocket: socket, State: store.StateRunning,
	}, nil, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(sess.Close)

	// No record was ever created, so Apply reports it missing, which is the same state a session removed
	// mid-retry leaves behind.
	if !mgr.recordShellPID(context.Background(), sess) {
		t.Error("recordShellPID() = false for a session with no record, want it treated as done")
	}
}
