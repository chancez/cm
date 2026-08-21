package server

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"
	"time"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// newSessionWithClients adopts a session and attaches n internal clients, returning their tokens.
//
// Built through attach rather than by writing clientSizes directly, so the entries are real attachments
// with the attached flag set: a reservation is deliberately excluded from every report here, and a test
// that hand-rolled its state could assert the wrong thing while passing.
func newSessionWithClients(t *testing.T, name string, n int) (*Session, []*attachToken) {
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

	toks := make([]*attachToken, 0, n)
	for i := range n {
		tok := sess.reserveClient()
		// A distinct pid per client, so a report naming the wrong one is visible rather than ambiguous.
		sess.noteClientIdentity(tok, "v1", int32(1000+i))
		if _, err := sess.attach(nil, tok); err != nil {
			t.Fatalf("attach() error = %v", err)
		}
		toks = append(toks, tok)
	}
	return sess, toks
}

// The client someone is using has to be identifiable, and typing is the only signal that identifies one.
//
// This is the question `cm clients list` and `cm clients upgrade --current` both rest on, and it cannot be
// answered any other way. A session's pty fans out to every attached client, so an escape sequence asking
// "which client are you" reaches all of them and is answered by whichever replies first, which is the
// duplicate-reply bug the query proxy exists to prevent. A command inside the session sees the pty as its
// stdout and has no client among its ancestors. Focus only reaches cm when the program inside enabled
// DECSET 1004, so a shell prompt reports nothing.
//
// Asserted through AttachedClients rather than by reading lastInputAt, because the mark is what a caller
// sees and computing it over the whole set is the part that can be wrong.
func TestActiveClientIsTheLastToType(t *testing.T) {
	sess, toks := newSessionWithClients(t, "active", 3)

	// A fixed clock, advanced by hand, so the ordering is stated rather than raced. Real keystroke times
	// differ by milliseconds and a test sleeping between them would be slow and still occasionally tie.
	now := time.Unix(1_700_000_000, 0)
	sess.mu.Lock()
	sess.clock = func() time.Time { return now }
	sess.mu.Unlock()

	// Nothing has typed yet, so no client is active. This is the state a freshly attached session is in,
	// and claiming one would be a guess: with three windows open and nothing typed, cm has no evidence
	// about which one someone is looking at.
	for _, c := range sess.AttachedClients() {
		if c.Active {
			t.Errorf("client pid %d marked active before anything was typed", c.PID)
		}
	}
	if _, ok := sess.ActiveClient(); ok {
		got, _ := sess.ActiveClient()
		t.Errorf("ActiveClient() = %+v, true before anything was typed, want false", got)
	}

	// The second client types.
	now = now.Add(time.Second)
	secondTyped := now
	sess.noteClientInput(toks[1])

	want := []AttachedClientInfo{
		{PID: 1000, Version: "v1"},
		{PID: 1001, Version: "v1", LastInputAt: secondTyped, Active: true},
		{PID: 1002, Version: "v1"},
	}
	got := sess.AttachedClients()
	// AttachedAt is set by attach from the real clock, so it is copied across rather than predicted. The
	// rest of every struct is asserted, which is what catches a field being right while its neighbour is
	// wrong.
	for i := range got {
		if i < len(want) {
			want[i].AttachedAt = got[i].AttachedAt
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AttachedClients() = %+v\nwant %+v", got, want)
	}

	active, ok := sess.ActiveClient()
	if !ok {
		t.Fatal("ActiveClient() reported none after a client typed")
	}
	wantActive := AttachedClientInfo{
		PID: 1001, Version: "v1", LastInputAt: secondTyped, AttachedAt: active.AttachedAt, Active: true,
	}
	if !reflect.DeepEqual(active, wantActive) {
		t.Errorf("ActiveClient() = %+v\nwant %+v", active, wantActive)
	}

	// The mark moves to whoever typed most recently, rather than sticking with the first to claim it.
	// Someone switching windows is the ordinary case this exists for.
	now = now.Add(time.Second)
	thirdTyped := now
	sess.noteClientInput(toks[2])

	active, ok = sess.ActiveClient()
	if !ok {
		t.Fatal("ActiveClient() reported none after the third client typed")
	}
	wantActive = AttachedClientInfo{
		PID: 1002, Version: "v1", LastInputAt: thirdTyped, AttachedAt: active.AttachedAt, Active: true,
	}
	if !reflect.DeepEqual(active, wantActive) {
		t.Errorf("ActiveClient() = %+v\nwant %+v", active, wantActive)
	}

	// And exactly one client carries it, which is the invariant a caller relies on when it takes the
	// first marked row it finds.
	marked := 0
	for _, c := range sess.AttachedClients() {
		if c.Active {
			marked++
		}
	}
	if marked != 1 {
		t.Errorf("%d clients marked active, want exactly 1:\n%+v", marked, sess.AttachedClients())
	}
}

// The input path must actually record it, which no unit test of noteClientInput can show.
//
// This is the failure mode that has already happened here for a neighbouring field: `Open` carried tags
// that were validated and then dropped, so every test that set the value itself passed while nothing on
// the real path did. The same shape applies twice over here, since the recording sits next to
// claimLeadership in a switch arm with several other cases.
//
// Driven through Attach with a real keystroke, and asserted on the pid the client declared, so a mark
// landing on the wrong attachment is visible.
func TestAttachRecordsInputAsActive(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("typed", "cat"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("typed")
	if !live {
		t.Fatal("session not adopted")
	}

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Held open, because a client that has gone is no longer attached and reports nothing at all, which
	// would make this pass for the wrong reason.
	stream := newHeldFakeStream(streamCtx,
		openReq(&serverv1.Open{
			Session: "typed", Rows: 24, Cols: 80, ClientVersion: "v1", ClientPid: 4242,
		}),
		// A plain character rather than an escape sequence: IsUserInput has to classify it as typing for
		// the recording to happen at all, and a mouse report or a query reply deliberately would not.
		inputReq("x"),
	)

	attached := make(chan error, 1)
	go func() { attached <- svc.Attach(streamCtx, stream) }()
	defer func() {
		stream.closeStream()
		if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("Attach() error = %v", err)
		}
	}()

	// Waited for rather than slept on, since the keystroke is processed asynchronously by the streaming
	// loop. A sleep here would be either slow or flaky depending on the machine.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if active, ok := sess.ActiveClient(); ok {
			if active.PID != 4242 {
				t.Fatalf("ActiveClient() = pid %d, want 4242, the client that typed", active.PID)
			}
			if active.LastInputAt.IsZero() {
				t.Error("ActiveClient().LastInputAt is zero after a keystroke")
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("no active client after a keystroke arrived on an attach stream; " +
				"the input path is not recording it")
		}
		time.Sleep(time.Millisecond)
	}
}

// A follower must never become the active client, because its input is dropped before it is recorded.
//
// Worth a test rather than an argument: read-only is enforced in the same switch arm the recording sits
// in, and the recording was deliberately not given its own readOnly check, on the grounds that dropped
// input can never reach it. That reasoning is only as good as the ordering, and the ordering is one line.
// A follower marked as the window someone is using would send `cm clients upgrade --current` at a pipe.
func TestFollowerIsNeverActive(t *testing.T) {
	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("follower-active", "cat"))
	rec.LogPath = "/unused"
	rec.State = store.StateRunning
	if err := st.Create(ctx, rec); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, live := mgr.Get("follower-active")
	if !live {
		t.Fatal("session not adopted")
	}

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream := newHeldFakeStream(streamCtx,
		openReq(&serverv1.Open{
			Session: "follower-active", Rows: 24, Cols: 80,
			ClientVersion: "v1", ClientPid: 4343, ReadOnly: true,
		}),
		inputReq("x"),
	)

	attached := make(chan error, 1)
	go func() { attached <- svc.Attach(streamCtx, stream) }()
	defer func() {
		stream.closeStream()
		if err := <-attached; err != nil && !errors.Is(err, io.EOF) {
			t.Errorf("Attach() error = %v", err)
		}
	}()

	// Waited for the attachment rather than for the input, since the assertion is that nothing happens and
	// there is no state change to wait on. Without this the test could pass by checking before the client
	// had attached at all.
	deadline := time.Now().Add(5 * time.Second)
	for sess.Clients() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("follower never attached")
		}
		time.Sleep(time.Millisecond)
	}
	// Then given the keystroke time to be wrongly recorded. A bare check after attach would race the
	// input and pass while the bug was present.
	time.Sleep(50 * time.Millisecond)

	if got, ok := sess.ActiveClient(); ok {
		t.Errorf("ActiveClient() = %+v after a follower sent input, want none: a follower's input is "+
			"dropped, so it cannot be the window someone is using", got)
	}
}

// A client that detaches must not leave the mark behind on a session nobody is typing in.
//
// The failure this guards against is a stale pid: releaseClientSize deletes the size entry, so the mark
// has to disappear with it rather than being remembered separately. A leftover entry would name a window
// that has closed, and `cm clients upgrade --current` would then ask nobody while reporting one asked.
func TestActiveClientForgottenOnDetach(t *testing.T) {
	sess, toks := newSessionWithClients(t, "detaching", 2)

	sess.noteClientInput(toks[0])
	if _, ok := sess.ActiveClient(); !ok {
		t.Fatal("ActiveClient() reported none after a client typed")
	}

	sess.releaseClientSize(toks[0])

	if got, ok := sess.ActiveClient(); ok {
		t.Errorf("ActiveClient() = %+v, true after the active client detached, want false", got)
	}
	// The remaining client is not promoted. It has typed nothing, so there is no evidence anyone is using
	// it, and inheriting the mark would say otherwise.
	for _, c := range sess.AttachedClients() {
		if c.Active {
			t.Errorf("client pid %d inherited the mark from a detached client", c.PID)
		}
	}
}

// The mark has to be recorded under every resize policy, not just the default.
//
// This is the reason noteClientInput exists rather than reusing Session.leader, which is the obvious
// shortcut: leadership is only maintained under ResizeLeader, since claimLeadership returns early for the
// other three policies. Reading the leader instead would have left `cm clients list` unable to mark
// anything for a user running resize_policy = "smallest", and the failure would be silent, since a
// session with one client looks identical either way.
func TestActiveClientIndependentOfResizePolicy(t *testing.T) {
	for _, policy := range []ResizePolicy{
		ResizeLeader, ResizeLastAttach, ResizeFirstAttach, ResizeSmallest,
	} {
		t.Run(string(policy), func(t *testing.T) {
			sess, toks := newSessionWithClients(t, "policy"+string(policy), 2)
			sess.SetResizePolicy(policy)

			// Through the same call the service makes on a keystroke, so this tests the recording rather
			// than a value written by hand.
			sess.noteClientInput(toks[1])

			active, ok := sess.ActiveClient()
			if !ok {
				t.Fatalf("ActiveClient() reported none under resize_policy %q; the mark must not depend "+
					"on the sizing policy", policy)
			}
			if active.PID != 1001 {
				t.Errorf("ActiveClient() = pid %d under resize_policy %q, want 1001",
					active.PID, policy)
			}

			// Sizing is unaffected, which is the separation being asserted: this records who is being
			// used and decides nothing about the pty's size.
			sess.mu.Lock()
			leader := sess.leader
			sess.mu.Unlock()
			if policy != ResizeLeader && leader != nil {
				t.Errorf("resize_policy %q gained a leader from noteClientInput, want none", policy)
			}
		})
	}
}

// Two clients with the same input time must leave the mark unset rather than picking by map order.
//
// Unreachable from real keystrokes, and worth pinning anyway: an injected clock that does not advance
// produces it, and Go randomizes map iteration, so the alternative is a mark that moves between two
// identical calls. A report that contradicts itself run to run is worse than one that declines to answer.
func TestActiveClientTieMarksNobody(t *testing.T) {
	sess, toks := newSessionWithClients(t, "tied", 2)

	frozen := time.Unix(1_700_000_000, 0)
	sess.mu.Lock()
	sess.clock = func() time.Time { return frozen }
	sess.mu.Unlock()

	sess.noteClientInput(toks[0])
	sess.noteClientInput(toks[1])

	if got, ok := sess.ActiveClient(); ok {
		t.Errorf("ActiveClient() = %+v, true for two clients tied at the same instant, want false", got)
	}
	// Repeated, because a single call could pick the same entry twice by chance while still being decided
	// by map order. Ten calls returning nothing is the invariant, not one.
	for range 10 {
		for _, c := range sess.AttachedClients() {
			if c.Active {
				t.Fatalf("client pid %d marked active on a tie", c.PID)
			}
		}
	}
}

// A reservation must never be the active client, even though it holds a size entry.
//
// reserveClient deliberately lets an unattached client win the sizing policy, so the entry this rule
// reads from exists before there is any stream to act on. cm has already shipped this conflation once:
// electing a reserved client as the terminal-query answerer left a program's query answered by nobody,
// because there was no stream to send the question down. An upgrade aimed at a reservation is the same
// shape of bug, with a repaint that never happens.
func TestReservationIsNeverActive(t *testing.T) {
	sess, _ := newSessionWithClients(t, "reserved-active", 1)

	tok := sess.reserveClient()
	sess.noteClientIdentity(tok, "v1", 9999)
	// A reservation cannot really type, since input arrives on an attach stream it does not have. Forced
	// here so the exclusion is tested where it matters: if this rule read only lastInputAt, the newest
	// timestamp would belong to something with nothing behind it.
	sess.noteClientInput(tok)

	if got, ok := sess.ActiveClient(); ok {
		t.Errorf("ActiveClient() = %+v for a bare reservation, want false: nothing is watching yet", got)
	}
	for _, c := range sess.AttachedClients() {
		if c.PID == 9999 {
			t.Errorf("reservation reported as an attached client: %+v", c)
		}
	}
}

// --current must upgrade the active client alone, leaving the session's other windows painted.
//
// The motivating case: upgrading the window a keybinding was pressed in. Naming the session asks every
// client attached to it, so with three windows open the other two repaint for a command that meant one.
func TestUpgradeClientsActiveOnly(t *testing.T) {
	sess, toks := newSessionWithClients(t, "upgrade-current", 3)

	sess.noteClientInput(toks[1])

	// force, so the version comparison does not decide this: every client here reports "v1", and the
	// point is which clients were considered rather than which builds they run.
	asked, alreadyCurrent := sess.UpgradeClients(true, "", true)
	if asked != 1 || alreadyCurrent != 0 {
		t.Errorf("UpgradeClients(activeOnly) = (%d, %d), want (1, 0)", asked, alreadyCurrent)
	}

	// And it is the right one. Checked through the eviction channels rather than the count, since a count
	// of 1 would also be produced by asking an arbitrary client.
	sess.mu.Lock()
	upgrading := make([]int32, 0, 1)
	for tok := range sess.upgrading {
		if cs := sess.clientSizes[tok]; cs != nil {
			upgrading = append(upgrading, cs.pid)
		}
	}
	sess.mu.Unlock()
	if !reflect.DeepEqual(upgrading, []int32{1001}) {
		t.Errorf("clients asked to upgrade = %v, want [1001], the one that typed", upgrading)
	}
}

// With no active client, --current must ask nobody rather than falling back to every client.
//
// The fallback is the tempting bug, and it inverts the request: a caller reached for --current precisely
// to spare the other windows, so upgrading all of them when cm cannot identify one is the worst available
// answer. Reporting zero asked lets the CLI say "no active client" instead.
func TestUpgradeClientsActiveOnlyWithNoActiveClient(t *testing.T) {
	sess, _ := newSessionWithClients(t, "upgrade-noactive", 3)

	asked, alreadyCurrent := sess.UpgradeClients(true, "", true)
	if asked != 0 || alreadyCurrent != 0 {
		t.Errorf("UpgradeClients(activeOnly) = (%d, %d) with nothing typed, want (0, 0): "+
			"a session whose active client cannot be named must not have every client upgraded",
			asked, alreadyCurrent)
	}
	sess.mu.Lock()
	pending := len(sess.upgrading)
	sess.mu.Unlock()
	if pending != 0 {
		t.Errorf("%d clients were asked to upgrade with no active client, want 0", pending)
	}
}

// Without --current every client is still asked, so the flag narrows rather than replacing the default.
//
// The control for the two tests above. Without it they would both pass against a build that never asks
// anyone, which is the shape of a test that cannot fail.
func TestUpgradeClientsWithoutActiveOnlyAsksEveryone(t *testing.T) {
	sess, toks := newSessionWithClients(t, "upgrade-all", 3)

	sess.noteClientInput(toks[1])

	asked, _ := sess.UpgradeClients(true, "", false)
	if asked != 3 {
		t.Errorf("UpgradeClients(force) asked %d clients, want 3", asked)
	}
}
