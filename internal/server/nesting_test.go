package server

import (
	"context"
	"testing"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// What a client actually sends as inside_session has to resolve to the parent.
//
// The bug this is the regression test for: a client sends the value of CM_SESSION, which is the ID with
// its sigil, and the server looked it up in the registry by bare ID. Every lookup failed, so nesting
// never engaged at all in a shipped binary: `cm list` never showed "(hosting ...)", a parent kept
// recording its child's directory as its own, and a `cm wait` on the parent could still be satisfied by
// the child. Nothing failed, because every test above calls beginHosting directly and the test covering
// the client's side used a name-shaped value.
//
// The name spelling is covered too, and is not hypothetical: a session created by an older server
// exported CM_SESSION as a name, and those sessions outlive the server that made them.
func TestHostingParentResolvesEitherSpelling(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	svc := &Service{mgr: mgr}
	ctx := context.Background()

	if err := st.Create(ctx, store.Session{ID: "parent", State: store.StateRunning}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	nameSession(t, st, "parent")
	parent := newNestedTestSession(t, nil)
	mgr.mu.Lock()
	mgr.sessions["parent"] = parent
	mgr.mu.Unlock()
	// Taken back out before the manager's own cleanup, which closes everything in the registry: this
	// session was built rather than started, so it has no pump to stop and Close panics on it.
	t.Cleanup(func() {
		mgr.mu.Lock()
		delete(mgr.sessions, "parent")
		mgr.mu.Unlock()
	})

	tests := []struct {
		name  string
		ref   string
		ownID string
		want  bool
	}{
		{
			// Built with the same function the server uses to tell a shim what to export, rather than
			// spelled out here. A literal "@parent" would keep passing if the exported spelling changed,
			// which is exactly how the two halves came apart: the consumer was tested against a value
			// the producer never produced.
			name:  "the ID with its sigil, which is what CM_SESSION holds",
			ref:   paths.FormatSessionID("parent"),
			ownID: "child",
			want:  true,
		},
		{
			name:  "a name, which is what an older server exported",
			ref:   "parent",
			ownID: "child",
			want:  true,
		},
		{
			// An attach from a real terminal, which is nearly every attach.
			name:  "nothing declared",
			ref:   "",
			ownID: "child",
			want:  false,
		},
		{
			// Attaching to the session you are already inside is not nesting. The guard for it used to
			// compare a reference against an ID, so it never matched.
			name:  "the session it is attaching to",
			ref:   "@parent",
			ownID: "parent",
			want:  false,
		},
		{
			// A parent that has exited, or one belonging to a different server. Not an error: there is
			// no bookkeeping here that could be wrong.
			name:  "a session this server does not have",
			ref:   "@gone",
			ownID: "child",
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, hosting := svc.hostingParent(ctx, tc.ref, tc.ownID)
			if hosting != tc.want {
				t.Fatalf("hostingParent(%q, %q) hosting = %v, want %v", tc.ref, tc.ownID, hosting, tc.want)
			}
			if tc.want && got != parent {
				t.Errorf("hostingParent(%q, %q) returned a different session than the parent",
					tc.ref, tc.ownID)
			}
			if !tc.want && got != nil {
				t.Errorf("hostingParent(%q, %q) returned a session, want nil", tc.ref, tc.ownID)
			}
		})
	}
}
