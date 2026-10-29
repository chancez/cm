package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// An Open carrying a command for a running session is refused only when the caller asked for that.
//
// `cm attach work -- make` on a running session attached, exited 0, and never ran make, which reads as
// success to whoever passed it. The refusal is opt-in on the wire so every other caller keeps the attach
// it had, and needs a command, since an attach without one drops nothing.
func TestOpenRefusesARunningSessionOnlyWhenAsked(t *testing.T) {
	cmd := []string{"/bin/sh", "-c", "true"}
	tests := []struct {
		name    string
		opts    OpenOptions
		wantErr error
	}{
		{
			name:    "error with a command refuses",
			opts:    OpenOptions{Ref: "running", Command: cmd, IfRunning: IfRunningError},
			wantErr: &RunningSessionError{Ref: "running"},
		},
		{
			name: "error without a command attaches",
			opts: OpenOptions{Ref: "running", IfRunning: IfRunningError},
		},
		{
			name: "attach with a command attaches",
			opts: OpenOptions{Ref: "running", Command: cmd, IfRunning: IfRunningAttach},
		},
		{
			// The zero value, which is what every internal caller and an older client's Open becomes.
			name: "unset with a command attaches",
			opts: OpenOptions{Ref: "running", Command: cmd},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, st, _ := newTestManager(t, nil)
			ctx := context.Background()
			rec := startShimFor(t, shimConfigFor("running", "sleep 5"))
			rec.State = store.StateRunning
			recordSession(t, st, rec)
			if err := mgr.Reconcile(ctx); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			sess, created, err := mgr.Open(ctx, tt.opts)
			if tt.wantErr != nil {
				var got *RunningSessionError
				if !errors.As(err, &got) || !reflect.DeepEqual(error(got), tt.wantErr) {
					t.Fatalf("Open() error = %v, want %v", err, tt.wantErr)
				}
				if sess != nil || created {
					t.Errorf("Open() = (%v, %v), want no session from a refusal", sess, created)
				}
				return
			}
			if err != nil {
				t.Fatalf("Open() error = %v, want an attach", err)
			}
			if created || sess.id != rec.ID {
				t.Errorf("Open() = (%s, created %v), want the running session %s", sess.id, created, rec.ID)
			}
		})
	}
}

// The second way a session is found running: recorded as running, missing from the registry, and adopted
// on the spot because its shim answers. It is refused there too, and stays adopted.
//
// Kept registered on a refusal because the adoption repaired something real, a live shell the registry
// had lost, and refusing this one caller is no reason to lose it again.
func TestOpenRefusesARunningSessionFoundOutsideTheRegistry(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()
	rec := startShimFor(t, shimConfigFor("unregistered", "sleep 5"))
	rec.State = store.StateRunning
	recordSession(t, st, rec)

	_, _, err := mgr.Open(ctx, OpenOptions{
		Ref:       "unregistered",
		Command:   []string{"/bin/sh", "-c", "true"},
		IfRunning: IfRunningError,
	})
	var got *RunningSessionError
	if !errors.As(err, &got) || !reflect.DeepEqual(got, &RunningSessionError{Ref: "unregistered"}) {
		t.Fatalf("Open() error = %v, want a RunningSessionError for unregistered", err)
	}

	mgr.mu.Lock()
	_, registered := mgr.sessions[rec.ID]
	mgr.mu.Unlock()
	if !registered {
		t.Error("the adopted session was dropped on refusal, want it kept in the registry")
	}
}

// The wire value maps to the manager's, and a value this build does not know refuses.
//
// Refusing the unknown is the safe direction: only a newer client sends one, asking for something else to
// happen to its command, and attaching instead would drop the command without a word.
func TestIfRunningFrom(t *testing.T) {
	tests := []struct {
		in   serverv1.IfRunning
		want IfRunning
	}{
		{serverv1.IfRunning_IF_RUNNING_UNSPECIFIED, IfRunningAttach},
		{serverv1.IfRunning_IF_RUNNING_ATTACH, IfRunningAttach},
		{serverv1.IfRunning_IF_RUNNING_ERROR, IfRunningError},
		{serverv1.IfRunning(99), IfRunningError},
	}
	for _, tt := range tests {
		if got := ifRunningFrom(tt.in); got != tt.want {
			t.Errorf("ifRunningFrom(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
