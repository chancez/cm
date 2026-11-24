package server

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/termmodes"
	"github.com/chancez/cm/internal/vt"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// modesScript sets the modes gr sets, then prints more than the shim in these tests retains, so a restart's
// replay starts after the bytes that set them. Then it waits, as gr does.
//
// It reads a line between the two, so the first server is known to have seen the setup before it is
// trimmed. On Linux the shell printed everything before the first server subscribed, the shim had already
// dropped the setup, and the first server recorded nothing; darwin's 1022-byte pty reads slowed the shell
// enough to hide it.
const modesScript = `printf '\033[?1049h\033[?1003h\033[?1006h'
read _
i=0
while [ $i -lt 400 ]; do echo "line $i of output that pushes the setup out"; i=$((i+1)); done
echo MODES_DONE
sleep 30`

// modesShimLogBytes is the shim's retention here, small enough that modesScript overruns it.
const modesShimLogBytes = 8 << 10

func realTerminal(rows, cols uint16) (Terminal, error) {
	return vt.NewSessionTerminal(rows, cols, 0)
}

// startModesSession runs modesScript under a first server and returns the manager, the store, the record and
// the session, once the session has consumed all of it.
func startModesSession(t *testing.T) (*Manager, *store.Store, store.Session, *Session) {
	t.Helper()
	mgr, st, _ := newTestManager(t, realTerminal)
	cfg := shimConfigFor("modes", modesScript)
	cfg.LogBytes = modesShimLogBytes
	rec := startShimFor(t, cfg)
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(context.Background()); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	first, ok := mgr.Get(rec.ID)
	if !ok {
		t.Fatal("session was not adopted by the first server")
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(first.Modes()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first server never recorded the script's modes")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := first.Write(context.Background(), []byte("\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	sub := first.recent.Subscribe(0)
	readUntil(t, sub, "MODES_DONE")
	sub.Close()
	return mgr, st, rec, first
}

// shimOldestSeq is the first byte a shim still retains.
func shimOldestSeq(t *testing.T, socket string) seq.Shim {
	t.Helper()
	conn, shim, err := dialShim(socket)
	if err != nil {
		t.Fatalf("dialShim() error = %v", err)
	}
	defer conn.Close()
	st, err := shim.State(context.Background(), &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	return seq.Shim(st.OldestSeq)
}

// A program's modes survive a server restart whose replay no longer holds the bytes that set them.
//
// This is the regression test. A gr three days old had set ?1049h and ?1003h at byte 11435653, the shim
// retains 4 MiB, and an adoption whose oldest retained byte was about 12.4M rebuilt a model on the main
// screen with no mouse reporting. Every client attaching afterwards was put in that state: keys reached gr,
// and the wheel scrolled kitty's scrollback.
func TestAdoptionKeepsModesTheReplayLost(t *testing.T) {
	mgr, _, rec, first := startModesSession(t)

	want := []termmodes.Mode{
		{Key: "?1003", Value: 1, SetAt: 0},
		{Key: "?1006", Value: 1, SetAt: 0},
		{Key: termmodes.AltScreen, Value: 1, SetAt: 0},
	}
	if got := first.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("first server's modes = %+v, want %+v", got, want)
	}
	// The precondition, without which this proves nothing: the setup is gone from what the shim holds.
	if oldest := shimOldestSeq(t, rec.ShimSocket); oldest == 0 {
		t.Fatal("the shim still holds the setup, so the replay would set the modes itself")
	}

	rec.LastSeq, rec.ClientSeq = releaseForRestart(t, first)
	rec.Modes = first.Modes()

	second, err := mgr.adopt(context.Background(), rec, rec.LastSeq, rec.ClientSeq, persistedRestore{})
	if err != nil {
		t.Fatalf("adopt() error = %v", err)
	}
	t.Cleanup(second.Close)

	// The modes carry their original positions, so the next restart applies them too.
	if got := second.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("adopted session's modes = %+v, want %+v", got, want)
	}
	got, err := second.term.Modes()
	if err != nil {
		t.Fatalf("Modes() error = %v", err)
	}
	if wantValues := termmodes.Values(want); !reflect.DeepEqual(got, wantValues) {
		t.Fatalf("adopted model's modes = %v, want %v", got, wantValues)
	}
}

// A stopping server records the modes with the resume points, so a clean restart does not depend on the
// last periodic write, which can be up to modesWriteInterval behind.
//
// The record is made stale by hand once persistModes has written it, since otherwise that write alone passes
// this: measured, with the write in Close removed, the first version of this test still passed.
func TestManagerCloseRecordsModes(t *testing.T) {
	mgr, st, rec, first := startModesSession(t)
	ctx := context.Background()
	want := first.Modes()
	if len(want) == 0 {
		t.Fatal("the session recorded no modes, so this would prove nothing")
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		got, err := st.Get(ctx, rec.ID)
		if err != nil {
			t.Fatalf("Get() error = %v", err)
		}
		if reflect.DeepEqual(got.Modes, want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("persistModes never wrote %+v; the record says %+v", want, got.Modes)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := st.Apply(ctx, rec.ID, store.Update{Modes: new([]termmodes.Mode{})}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if err := mgr.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	got, err := st.Get(ctx, rec.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if !reflect.DeepEqual(got.Modes, want) {
		t.Fatalf("stored modes = %+v, want %+v", got.Modes, want)
	}
}

// Modes are written at once, then at most once per modesWriteInterval, with what changed meanwhile coalesced
// into one write of the latest. nvim hides and shows the cursor around every redraw, so a write per change
// would be a sqlite write per frame.
func TestPersistModesWritesAtMostOncePerInterval(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		st, err := store.Open(ctx, filepath.Join(t.TempDir(), "cm.db"))
		if err != nil {
			t.Fatalf("store.Open() error = %v", err)
		}
		defer st.Close()
		if err := st.Create(ctx, store.Session{ID: "w"}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		m := &Manager{store: st, log: cmlog.Discard()}
		sess := &Session{id: "w", done: make(chan struct{}), modesChanged: make(chan struct{}, 1)}
		// Deferred, so a failure still ends persistModes rather than leaving the bubble deadlocked.
		defer func() {
			close(sess.done)
			synctest.Wait()
		}()

		stored := func() []termmodes.Mode {
			t.Helper()
			synctest.Wait()
			rec, err := st.Get(ctx, "w")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			return rec.Modes
		}
		change := func(modes []termmodes.Mode) {
			sess.mu.Lock()
			sess.modes = modes
			sess.mu.Unlock()
			select {
			case sess.modesChanged <- struct{}{}:
			default:
			}
		}
		hidden := []termmodes.Mode{{Key: "?25", Value: 0, SetAt: 10}}
		alt := []termmodes.Mode{{Key: termmodes.AltScreen, Value: 1, SetAt: 20}}
		both := []termmodes.Mode{{Key: "?25", Value: 0, SetAt: 30}, {Key: termmodes.AltScreen, Value: 1, SetAt: 20}}

		sess.modes = hidden
		go m.persistModes(sess, nil)
		if got := stored(); !reflect.DeepEqual(got, hidden) {
			t.Fatalf("after start, stored = %+v, want %+v", got, hidden)
		}

		change(alt)
		if got := stored(); !reflect.DeepEqual(got, alt) {
			t.Fatalf("after the first change, stored = %+v, want %+v", got, alt)
		}
		change(nil)
		change(both)
		if got := stored(); !reflect.DeepEqual(got, alt) {
			t.Fatalf("inside the interval, stored = %+v, want %+v unchanged", got, alt)
		}
		time.Sleep(modesWriteInterval)
		if got := stored(); !reflect.DeepEqual(got, both) {
			t.Fatalf("after the interval, stored = %+v, want %+v", got, both)
		}
	})
}
