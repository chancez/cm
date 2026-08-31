package server

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A session must be able to say what is attached to it, not just how many things are.
//
// The count was all the server had, and it made a real investigation slow. After losing a session the
// only way to find out what was attached was `ps` plus lsof, comparing binary inodes by hand to work
// out which clients were running which build. Since a shim outlives its server by design, a healthy
// install spans several builds at once, so "which client is which" is a routine question rather than an
// exotic one: the incident that prompted this had twelve distinct builds across twenty-six sessions.
//
// Asserted through the RPC rather than by calling noteClientIdentity directly, because the wiring is
// the part that broke before for other fields: `Open` carried tags that were validated and dropped, so
// a test that sets the value itself would pass while nothing on the real path did.
func TestAttachRecordsClientIdentity(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("identified", "sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	sess, live := mgr.Get("identified")
	if !live {
		t.Fatal("session not adopted")
	}
	// A fixed clock, so the attach time is asserted rather than approximated. The session already
	// supports one for query expiry.
	attachedAt := time.Unix(1_700_000_000, 0)
	sess.mu.Lock()
	sess.clock = func() time.Time { return attachedAt }
	sess.mu.Unlock()

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Held open, since a client that has gone is no longer attached and would report nothing.
	stream := newHeldFakeStream(streamCtx, openReq(&serverv1.Open{
		Session:       "identified",
		Rows:          24,
		Cols:          80,
		ClientVersion: "v0.1.2-9-g4352aa4",
		ClientPid:     4242,
	}))

	attached := make(chan error, 1)
	go func() { attached <- svc.Attach(streamCtx, stream) }()

	// Waited for rather than slept on: the attachment exists once the client is counted.
	deadline := time.Now().Add(5 * time.Second)
	for sess.Clients() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("client never attached")
		}
		time.Sleep(time.Millisecond)
	}

	got := sess.AttachedClients()
	want := []AttachedClientInfo{{
		PID:        4242,
		Version:    "v0.1.2-9-g4352aa4",
		ReadOnly:   false,
		AttachedAt: attachedAt,
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachedClients() = %+v\nwant %+v", got, want)
	}

	stream.closeStream()
	if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	// And a client that has gone is not reported. A stale entry would be worse than no entry, since the
	// whole purpose is answering "what was attached" after something went wrong.
	deadline = time.Now().Add(5 * time.Second)
	for len(sess.AttachedClients()) > 0 {
		if time.Now().After(deadline) {
			t.Errorf("AttachedClients() = %+v after the client left, want empty",
				sess.AttachedClients())
			break
		}
		time.Sleep(time.Millisecond)
	}
}

// A reservation must not be reported as an attached client.
//
// reserveClient exists so a session can be resized to a client's size before its screen is serialized,
// which means a reservation deliberately holds a size and can win the sizing policy while still being
// only a reservation. Reporting one as attached would say a session has something watching it when
// nothing does, and that exact conflation is a bug cm has already had: counting a reserved client as an
// answerer left a program's terminal query answered by nobody.
func TestReservationIsNotReportedAsAttached(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("reserved", "sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("reserved")
	if !live {
		t.Fatal("session not adopted")
	}

	tok := sess.reserveClient()
	sess.noteClientIdentity(tok, "v0.1.2", 4242, capability.Set{})

	if got := sess.AttachedClients(); len(got) != 0 {
		t.Errorf("AttachedClients() = %+v for a bare reservation, want empty: nothing is watching "+
			"this session yet", got)
	}

	// Sizing still counts it, which is the distinction rather than an inconsistency.
	sess.mu.Lock()
	_, sized := sess.clientSizes[tok]
	sess.mu.Unlock()
	if !sized {
		t.Error("a reservation lost its sizing entry, which is what reserveClient exists to hold")
	}
}
