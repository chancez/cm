package server

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// upgradableSession adopts a live session with one attached client, returning the stream serving it.
//
// Held open, because a client that has gone is not attached and there would be nothing to upgrade.
func upgradableSession(
	t *testing.T, name, version string,
) (*Manager, *Session, *fakeAttachStream, chan error, context.CancelFunc) {
	t.Helper()

	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor(name, "sleep 30"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get(name)
	if !live {
		t.Fatal("session not adopted")
	}

	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	stream := newHeldFakeStream(streamCtx, openReq(&serverv1.Open{
		Session:       name,
		Rows:          24,
		Cols:          80,
		ClientVersion: version,
		ClientPid:     4242,
	}))

	svc := NewService(mgr)
	attached := make(chan error, 1)
	go func() { attached <- svc.Attach(streamCtx, stream) }()

	deadline := time.Now().Add(5 * time.Second)
	for sess.Clients() == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("client never attached")
		}
		time.Sleep(time.Millisecond)
	}
	return mgr, sess, stream, attached, cancel
}

// A client asked to upgrade must be told so, rather than told to detach.
//
// The distinction is the whole feature. Both send Detached and both close the stream, so a client that
// cannot tell them apart exits and the window closes: an upgrade would become the exact thing it exists
// to avoid. The flag is what says "come back", so it is asserted on the wire rather than inferred from
// the client having gone.
func TestUpgradeClientsAsksClientToComeBack(t *testing.T) {
	mgr, sess, stream, attached, cancel := upgradableSession(t, "upgradable", "old-build")
	defer cancel()

	mgr.SetVersion("new-build")
	svc := NewService(mgr)
	resp, err := svc.UpgradeClients(context.Background(), &serverv1.UpgradeClientsRequest{
		Sessions: []string{"upgradable"},
	})
	if err != nil {
		t.Fatalf("UpgradeClients() error = %v", err)
	}
	if got := resp.Asked["upgradable"]; got != 1 {
		t.Errorf("asked = %d, want 1", got)
	}

	if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	// The last event must be a Detached carrying the upgrade flag. Without the flag the client exits.
	var detached *serverv1.Detached
	for _, sent := range stream.sent() {
		if d := sent.GetDetached(); d != nil {
			detached = d
		}
	}
	if detached == nil {
		t.Fatal("client was never told to let go, so it would sit on a closed stream")
	}
	if !detached.Upgrade {
		t.Error("Detached.Upgrade = false, so the client exits and the window closes " +
			"instead of coming back on the new build")
	}

	// And the session is still running, since this is a detach rather than a kill.
	if ended, _ := sess.Ended(); ended {
		t.Error("session ended, want it still running: an upgrade must not touch the shell")
	}
}

// A client already on the server's build is left alone, so upgrading twice does not repaint every window.
func TestUpgradeClientsSkipsCurrentClients(t *testing.T) {
	mgr, _, stream, attached, cancel := upgradableSession(t, "current", "same-build")
	defer cancel()

	mgr.SetVersion("same-build")
	svc := NewService(mgr)
	resp, err := svc.UpgradeClients(context.Background(), &serverv1.UpgradeClientsRequest{
		Sessions: []string{"current"},
	})
	if err != nil {
		t.Fatalf("UpgradeClients() error = %v", err)
	}
	if got := resp.Asked["current"]; got != 0 {
		t.Errorf("asked = %d, want 0: a client on the server's build has nothing to upgrade to", got)
	}
	if got := resp.AlreadyCurrent["current"]; got != 1 {
		t.Errorf("already_current = %d, want 1", got)
	}

	// Nothing was sent, so the client is still attached and streaming.
	for _, sent := range stream.sent() {
		if sent.GetDetached() != nil {
			t.Error("a client already on the server's build was asked to let go")
		}
	}

	// Released here rather than by the upgrade, which is the point: it was still attached.
	stream.closeStream()
	if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}
}

// --force asks a current client anyway, which is how clients are restarted after a config change.
func TestUpgradeClientsForceAsksCurrentClients(t *testing.T) {
	mgr, _, stream, attached, cancel := upgradableSession(t, "forced", "same-build")
	defer cancel()

	mgr.SetVersion("same-build")
	svc := NewService(mgr)
	resp, err := svc.UpgradeClients(context.Background(), &serverv1.UpgradeClientsRequest{
		Sessions: []string{"forced"},
		Force:    true,
	})
	if err != nil {
		t.Fatalf("UpgradeClients() error = %v", err)
	}
	if got := resp.Asked["forced"]; got != 1 {
		t.Errorf("asked = %d, want 1 with --force", got)
	}
	// Nothing is skipped with force, so the map is omitted entirely rather than sent holding a zero.
	if resp.AlreadyCurrent != nil {
		t.Errorf("already_current = %v, want nil with --force: nothing is skipped", resp.AlreadyCurrent)
	}

	if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}
	found := false
	for _, sent := range stream.sent() {
		if d := sent.GetDetached(); d != nil && d.Upgrade {
			found = true
		}
	}
	if !found {
		t.Error("--force did not ask a current client to upgrade")
	}
}

// A client that reported no version is never skipped.
//
// An unknown build is more likely to be old than current: the field exists precisely because older
// clients did not report one. Skipping it would leave the stalest client in place, which is the failure
// this command exists to fix, so the ambiguous case resolves toward asking.
func TestUpgradeClientsAsksClientsWithNoVersion(t *testing.T) {
	mgr, _, stream, attached, cancel := upgradableSession(t, "unknown", "")
	defer cancel()

	mgr.SetVersion("new-build")
	svc := NewService(mgr)
	resp, err := svc.UpgradeClients(context.Background(), &serverv1.UpgradeClientsRequest{
		Sessions: []string{"unknown"},
	})
	if err != nil {
		t.Fatalf("UpgradeClients() error = %v", err)
	}
	if got := resp.Asked["unknown"]; got != 1 {
		t.Errorf("asked = %d, want 1: a client that reported no build is not known to be current", got)
	}

	if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}
	found := false
	for _, sent := range stream.sent() {
		if d := sent.GetDetached(); d != nil && d.Upgrade {
			found = true
		}
	}
	if !found {
		t.Error("a client with no reported version was not asked to upgrade")
	}
}

// A plain detach must not be marked as an upgrade, or every `cm detach` would silently reattach.
//
// The inverse of the first test, and the reason the upgrade flag lives in a side table written before
// the eviction channel closes rather than being inferred: if the two paths shared state incorrectly, a
// detach would come back and the command would appear not to work at all.
func TestDetachIsNotReportedAsAnUpgrade(t *testing.T) {
	mgr, _, stream, attached, cancel := upgradableSession(t, "plain", "old-build")
	defer cancel()

	mgr.SetVersion("new-build")
	svc := NewService(mgr)
	if _, err := svc.Detach(context.Background(), &serverv1.DetachRequest{
		Sessions: []string{"plain"},
	}); err != nil {
		t.Fatalf("Detach() error = %v", err)
	}

	if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}
	var detached *serverv1.Detached
	for _, sent := range stream.sent() {
		if d := sent.GetDetached(); d != nil {
			detached = d
		}
	}
	if detached == nil {
		t.Fatal("client was never told to let go")
	}
	if detached.Upgrade {
		t.Error("Detached.Upgrade = true for a plain detach, so the client would come back " +
			"and `cm detach` would appear to do nothing")
	}
}
