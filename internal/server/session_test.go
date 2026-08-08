package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/store"
)

// fakeTerminal stands in for the libghostty-backed emulator. It records what it was fed and
// returns a canned restore blob, which is all the server's attach logic actually depends on.
type fakeTerminal struct {
	mu       sync.Mutex
	written  []byte
	restore  []byte
	writeErr error
	closed   bool
	rows     uint16
	cols     uint16
	pending  [][]byte
	title    string
	pwd      string
}

func (f *fakeTerminal) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = append(f.written, p...)
	return nil
}

func (f *fakeTerminal) Restore() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restore, nil
}

func (f *fakeTerminal) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
}

func (f *fakeTerminal) Resize(rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows, f.cols = rows, cols
	return nil
}

func (f *fakeTerminal) TakePending() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.pending
	f.pending = nil
	return out
}

func (f *fakeTerminal) Title() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.title
}

func (f *fakeTerminal) Pwd() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pwd
}

func (f *fakeTerminal) Plain() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written, nil
}

func (f *fakeTerminal) VT() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written, nil
}

func (f *fakeTerminal) HTML() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte("<pre>"), append(f.written, []byte("</pre>")...)...), nil
}

func (f *fakeTerminal) Size() (uint16, uint16) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rows, f.cols
}

func (f *fakeTerminal) Written() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return string(f.written)
}

// shortTempDir keeps socket paths under the sockaddr_un limit, which t.TempDir() exceeds
// because it embeds the test name.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cm")
	if err != nil {
		t.Fatalf("MkdirTemp() error = %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// startShimFor runs a real shim so the server can be exercised against the actual protocol.
// The shim is genuinely cheap to start and the socket lifecycle is where things break, so
// this is worth doing for real rather than faking.
func startShimFor(t *testing.T, cfg shim.Config) store.Session {
	t.Helper()

	socket := filepath.Join(shortTempDir(t), "s.sock")
	l, err := shim.Listen(socket)
	if err != nil {
		t.Fatalf("shim.Listen() error = %v", err)
	}
	sess, err := shim.Start(cfg)
	if err != nil {
		l.Close()
		t.Fatalf("shim.Start() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = shim.Serve(ctx, l, shim.NewService(sess))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	// The shim binds before spawning the shell, so a connectable socket means it is ready.
	waitSocket(t, socket)

	return store.Session{Name: cfg.Session, ShimSocket: socket, Rows: int(cfg.Rows), Cols: int(cfg.Cols)}
}

func waitSocket(t *testing.T, socket string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(socket); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear", socket)
}

// readUntil accumulates from a reader until want appears.
func readUntil(t *testing.T, r *seqlog.Reader, want string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	for {
		c, err := r.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for %q: %v (got %q)", want, err, sb.String())
		}
		sb.Write(c.Data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
}

func TestSessionFeedsTerminalModel(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "term",
		Command: []string{"/bin/sh", "-c", "echo MODELED; sleep 5"},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)
	readUntil(t, r, "MODELED")

	// The terminal model must see the same bytes the client does, or a restore would show a
	// screen that never existed.
	if got := term.Written(); !strings.Contains(got, "MODELED") {
		t.Errorf("terminal model saw %q, want it to contain %q", got, "MODELED")
	}
}

// A fresh attach replays serialized screen state; a resuming one does not, because its
// terminal already shows the session and repainting would duplicate what is on screen.
func TestAttachReplaysStateOnlyOnFreshAttach(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "restore",
		Command: []string{"/bin/sh", "-c", "echo HELLO; sleep 5"},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Let output accumulate so there is state worth restoring.
	warm := sess.recent.Subscribe(0)
	readUntil(t, warm, "HELLO")
	warm.Close()

	fresh, restore, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("fresh attach() error = %v", err)
	}
	defer sess.detach(fresh)
	if string(restore) != "RESTORED" {
		t.Errorf("fresh attach restore = %q, want %q", restore, "RESTORED")
	}

	from := uint64(0)
	resumed, restore, err := sess.attach(&from)
	if err != nil {
		t.Fatalf("resumed attach() error = %v", err)
	}
	defer sess.detach(resumed)
	if len(restore) != 0 {
		t.Errorf("resumed attach restore = %q, want nothing: the client already has the screen", restore)
	}
	if got := resumed.Position(); got != from {
		t.Errorf("resumed reader position = %d, want %d", got, from)
	}
}

// Without a terminal model there is nothing to serialize, so a fresh client is served from
// retained output instead. Worse than a real repaint, but better than a blank screen.
func TestAttachWithoutTerminalReplaysRecentOutput(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "noterm",
		Command: []string{"/bin/sh", "-c", "echo EARLIER; sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	warm := sess.recent.Subscribe(0)
	readUntil(t, warm, "EARLIER")
	warm.Close()

	// Attaching after that output was produced must still show it.
	r, restore, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)
	if len(restore) != 0 {
		t.Errorf("restore = %q, want nothing when there is no terminal model", restore)
	}
	if got := readUntil(t, r, "EARLIER"); !strings.Contains(got, "EARLIER") {
		t.Errorf("output = %q, want earlier output replayed", got)
	}
}

// Multiple clients share a session, so each must see the full stream independently.
func TestMultipleClientsEachSeeOutput(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "multi",
		Command: []string{"/bin/sh", "-c", "echo SHARED; sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	r1, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	defer sess.detach(r1)
	r2, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(r2)

	if n := sess.Clients(); n != 2 {
		t.Errorf("Clients() = %d, want 2", n)
	}
	for i, r := range []*seqlog.Reader{r1, r2} {
		if got := readUntil(t, r, "SHARED"); !strings.Contains(got, "SHARED") {
			t.Errorf("client %d saw %q, want SHARED", i, got)
		}
	}

	sess.detach(r1)
	if n := sess.Clients(); n != 1 {
		t.Errorf("Clients() after detach = %d, want 1", n)
	}
}

// When the shell exits, readers must reach ErrClosed after draining, and the exit status must
// be available. That combination is how a client learns to stop rather than reconnect.
func TestSessionEndsWhenShellExits(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "exits",
		Command: []string{"/bin/sh", "-c", "echo BYE; exit 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	var lastErr error
	for {
		c, err := r.Next(ctx)
		if err != nil {
			lastErr = err
			break
		}
		sb.Write(c.Data)
	}

	if !errors.Is(lastErr, seqlog.ErrClosed) {
		t.Errorf("final error = %v, want seqlog.ErrClosed", lastErr)
	}
	if !strings.Contains(sb.String(), "BYE") {
		t.Errorf("output = %q, want the shell's final output preserved", sb.String())
	}

	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not report done")
	}
	ended, code := sess.Ended()
	if !ended || code != 5 {
		t.Errorf("Ended() = (%v, %d), want (true, 5)", ended, code)
	}
}

// Attaching to a session that has already ended must fail rather than hand back a reader that
// will never produce anything.
func TestAttachToEndedSessionFails(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gone",
		Command: []string{"/bin/sh", "-c", "exit 0"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end")
	}

	if _, _, err := sess.attach(nil); !errors.Is(err, ErrSessionGone) {
		t.Errorf("attach() error = %v, want ErrSessionGone", err)
	}
}

// A terminal model that fails must not take the session down: live output still works, only
// restores are lost.
func TestTerminalWriteFailureDoesNotKillSession(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "termfail",
		Command: []string{"/bin/sh", "-c", "echo STILLWORKS; sleep 5"},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{writeErr: errors.New("emulator exploded")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)

	if got := readUntil(t, r, "STILLWORKS"); !strings.Contains(got, "STILLWORKS") {
		t.Errorf("output = %q, want output to keep flowing despite the model failing", got)
	}
	if ended, _ := sess.Ended(); ended {
		t.Error("session ended because the terminal model failed, want it to keep running")
	}
}

// LastSeq is the resume point a restarting server uses, so it must track consumption.
func TestLastSeqAdvancesWithOutput(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "seq",
		Command: []string{"/bin/sh", "-c", "echo COUNTED; sleep 5"},
		Rows:    24, Cols: 80,
	})

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	if got := sess.LastSeq(); got != 0 {
		t.Errorf("initial LastSeq() = %d, want 0", got)
	}

	r, _, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(r)
	out := readUntil(t, r, "COUNTED")

	if got := sess.LastSeq(); got < uint64(len(out)) {
		t.Errorf("LastSeq() = %d, want at least %d after consuming %q",
			got, len(out), out)
	}
}

// A server adopting a session mid-stream must keep the shim's numbering, or a position named
// by a client would mean different things on each hop.
func TestSessionPreservesShimSequenceNumbering(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "numbering",
		Command: []string{"/bin/sh", "-c", "echo FIRST; sleep 0.2; echo SECOND; sleep 5"},
		Rows:    24, Cols: 80,
	})

	// Consume the early output through one session, then adopt again from where it stopped,
	// as a restarting server would.
	first, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	r1 := first.recent.Subscribe(0)
	readUntil(t, r1, "FIRST")
	r1.Close()
	resumeFrom := first.LastSeq()
	first.Close()

	second, err := newSession(rec, nil, resumeFrom)
	if err != nil {
		t.Fatalf("second newSession() error = %v", err)
	}
	defer second.Close()

	r2, _, err := second.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer second.detach(r2)

	if got := r2.Position(); got < resumeFrom {
		t.Errorf("resumed reader position = %d, want at least the resume point %d",
			got, resumeFrom)
	}
	if got := readUntil(t, r2, "SECOND"); strings.Contains(got, "FIRST") {
		t.Errorf("resumed output = %q, want it to exclude already-consumed output", got)
	}
}

// shimConfigFor builds a shim config running a shell command, keeping the test cases above
// focused on what they assert rather than on boilerplate.
func shimConfigFor(name, script string) shim.Config {
	return shim.Config{
		Session: name,
		Command: []string{"/bin/sh", "-c", script},
		Rows:    24,
		Cols:    80,
	}
}

// A newly attached client must receive current metadata immediately rather than waiting for the
// shell to report again, since a shell reports its directory once per prompt.
func TestSubscribeMetadataDeliversCurrentValues(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("meta", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Simulate the shell having already reported before this client arrived.
	sess.mu.Lock()
	sess.title = "existing-title"
	sess.cwd = osc.Cwd{Path: "/existing/path", IsLocal: true}
	sess.mu.Unlock()

	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)

	select {
	case got := <-sub.ch:
		want := Metadata{Title: "existing-title", Cwd: osc.Cwd{Path: "/existing/path", IsLocal: true}}
		if got != want {
			t.Errorf("metadata = %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Error("no metadata delivered on subscribe; a client would show a stale title")
	}
}

// A slow reader must see the newest values rather than a backlog, and must never stall the output
// pump that publishes them.
func TestPublishMetadataCoalesces(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("coalesce", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)

	// Publish several times without reading in between.
	for i, title := range []string{"first", "second", "third"} {
		sess.publishMetadata(Metadata{Title: title})
		_ = i
	}

	select {
	case got := <-sub.ch:
		if got.Title != "third" {
			t.Errorf("metadata title = %q, want %q: a slow reader should see the newest value",
				got.Title, "third")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no metadata delivered")
	}

	// And nothing stale queued behind it.
	select {
	case extra := <-sub.ch:
		t.Errorf("a second value %+v was queued, want only the newest retained", extra)
	default:
	}
}

// Publishing must not block when a subscriber never reads, or the output pump would wedge and the
// whole session would stop.
func TestPublishMetadataDoesNotBlockOnUnreadSubscriber(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("noblock", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			sess.publishMetadata(Metadata{Title: "t" + string(rune('a'+i%26))})
		}
	}()

	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publishMetadata blocked on a subscriber that never reads")
	}
}
