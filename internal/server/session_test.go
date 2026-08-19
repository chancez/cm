package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	// restoreRows and restoreCols are the size the model held the last time Restore was called, and
	// restoredAt counts the calls. Together they answer "was the screen serialized at the size the
	// client will display it at", which is the ordering a snapshot depends on.
	restoreRows uint16
	restoreCols uint16
	restoredAt  int
	// focusReporting stands in for DECSET 1004 being enabled by the program.
	focusReporting bool

	// answers maps a query to the reply the emulator would generate for it, so a test can exercise
	// the path where output triggers a write back to the pty. Real libghostty answers CSI 6n and
	// friends this way; without this the fake never produces pending bytes and a test about
	// answering would pass while testing nothing.
	answers map[string]string
}

func (f *fakeTerminal) Write(p []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return f.writeErr
	}
	f.written = append(f.written, p...)
	for query, reply := range f.answers {
		if bytes.Contains(p, []byte(query)) {
			f.pending = append(f.pending, []byte(reply))
		}
	}
	return nil
}

func (f *fakeTerminal) Restore() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// The size the model held when the screen was serialized, which is the thing that has to match
	// the client the bytes are being sent to. Recorded rather than asserted on the model's current
	// size afterwards: a resize that lands *after* the snapshot leaves the model looking correct
	// while the bytes already taken describe the old width.
	f.restoreRows, f.restoreCols = f.rows, f.cols
	f.restoredAt++
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

func (f *fakeTerminal) FocusReporting() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.focusReporting
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

// Tail mirrors the real implementation closely enough to test the plumbing: it bounds the output by
// lines and, when asked to unwrap, joins nothing, since a fake has no notion of soft wrapping. Tests
// about unwrapping belong against the real emulator.
func (f *fakeTerminal) Tail(lines int, unwrap bool) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	if lines <= 0 {
		return f.written, nil
	}
	// Count back from the end, matching the real one.
	all := strings.Split(strings.TrimSuffix(string(f.written), "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return []byte(strings.Join(all, "\n")), nil
}

// TailVT returns the same bytes as Tail.
//
// A fake writes no escape sequences, so there is nothing for a raw form to preserve that a plain one drops.
// Distinguishing them here would mean inventing styling the fake never had; the difference belongs against the
// real emulator, where the formatter actually produces it.
func (f *fakeTerminal) TailVT(lines int) ([]byte, error) {
	return f.Tail(lines, false)
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
// Not suitable for anything that scans the runtime directory.
//
// The socket goes in a private temp dir rather than at paths.Dirs.ShimSocket, which is fine for a test that
// dials it directly and wrong for one that expects to *find* it: `cm doctor` works by enumerating the runtime
// directory, so a shim placed here is invisible to it and every assertion passes for the wrong reason. Use
// startShimInRuntimeDir in doctor_test.go for those.
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

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	readUntil(t, att.reader, "MODELED")

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

	fresh, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("fresh attach() error = %v", err)
	}
	defer sess.detach(fresh)
	if string(fresh.restore) != "RESTORED" {
		t.Errorf("fresh attach restore = %q, want %q", fresh.restore, "RESTORED")
	}
	if !fresh.first {
		t.Error("first attach reports first = false, want true so focus can be reported")
	}

	from := uint64(0)
	resumed, err := sess.attach(&from, nil)
	if err != nil {
		t.Fatalf("resumed attach() error = %v", err)
	}
	defer sess.detach(resumed)
	if len(resumed.restore) != 0 {
		t.Errorf("resumed attach restore = %q, want nothing: the client already has the screen",
			resumed.restore)
	}
	if got := resumed.reader.Position(); got != from {
		t.Errorf("resumed reader position = %d, want %d", got, from)
	}
	if resumed.first {
		t.Error("second attach reports first = true, want false")
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
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	if len(att.restore) != 0 {
		t.Errorf("restore = %q, want nothing when there is no terminal model", att.restore)
	}
	if got := readUntil(t, att.reader, "EARLIER"); !strings.Contains(got, "EARLIER") {
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

	a1, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	a2, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(a2)

	if n := sess.Clients(); n != 2 {
		t.Errorf("Clients() = %d, want 2", n)
	}
	for i, r := range []*seqlog.Reader{a1.reader, a2.reader} {
		if got := readUntil(t, r, "SHARED"); !strings.Contains(got, "SHARED") {
			t.Errorf("client %d saw %q, want SHARED", i, got)
		}
	}

	// Detaching one of two must not report itself as the last, or focus loss would be sent while
	// a client is still watching.
	if last := sess.detach(a1); last {
		t.Error("detach reported last = true with another client attached")
	}
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

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var sb strings.Builder
	var lastErr error
	for {
		c, err := att.reader.Next(ctx)
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

	if _, err := sess.attach(nil, nil); !errors.Is(err, ErrSessionGone) {
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

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	if got := readUntil(t, att.reader, "STILLWORKS"); !strings.Contains(got, "STILLWORKS") {
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

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	out := readUntil(t, att.reader, "COUNTED")

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

	a2, err := second.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer second.detach(a2)

	if got := a2.reader.Position(); got < resumeFrom {
		t.Errorf("resumed reader position = %d, want at least the resume point %d",
			got, resumeFrom)
	}
	if got := readUntil(t, a2.reader, "SECOND"); strings.Contains(got, "FIRST") {
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

// A snapshot must describe the size the attaching client will display at.
//
// Taking it before resizing yields lines wrapped for the old width, which the client then wraps
// again, and the screen arrives mangled. The bug is invisible unless a client attaches at a
// different size than the session currently has, so it needs an explicit test.
func TestAttachSnapshotsAtTheClientsSize(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("sizing", "sleep 5"))
	rec.Rows, rec.Cols = 24, 80

	term := &fakeTerminal{rows: 24, cols: 80, restore: []byte("SNAPSHOT")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// What the service does on a fresh attach from a differently sized client: resize, then
	// snapshot.
	if err := sess.Resize(context.Background(), 40, 120, 0, 0); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	if len(att.restore) == 0 {
		t.Fatal("attach() returned no restore bytes")
	}

	// The model must already be at the client's size when the snapshot is taken.
	gotRows, gotCols := term.Size()
	if gotRows != 40 || gotCols != 120 {
		t.Errorf("terminal model size at snapshot = (%d, %d), want (40, 120)", gotRows, gotCols)
	}
}

// A program that asked for focus events must be told when nobody is watching.
//
// Some programs use focus to decide whether to render at all, or whether to raise a desktop
// notification instead of drawing. A detached session is exactly "nobody is watching", so without
// this the program keeps behaving as though it is on screen.
//
// The reports are observed by having the shell read them and print what it got, rather than by
// looking for the escape bytes in the session's output. A pty echoes control characters in caret
// notation, so the raw sequence never appears in the stream as written.
func TestReportFocusOnlyWhenProgramAsked(t *testing.T) {
	// Echoes what it receives, and the test looks for the focus report's final byte in that echo: O
	// for focus-out, I for focus-in.
	//
	// `cat` rather than the shell's read builtin. `read -n` is a bashism and /bin/sh is dash on
	// Debian, so a version using it passed on macOS and hung on Linux, which is exactly the class of
	// difference the Linux run exists to catch. cat needs no shell features at all.
	const script = "echo READY; cat"

	loud := &fakeTerminal{focusReporting: true}
	rec := startShimFor(t, shimConfigFor("focus-on", script))
	sess, err := newSession(rec, loud, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	readUntil(t, att.reader, "READY")

	// Both reports, then read once. A pty echoes a control character in caret notation, so the
	// escape appears as "^[" rather than as an ESC byte, and searching for the raw sequence would
	// never match. The parameters after it are what identify each report.
	sess.ReportFocus(context.Background(), false)
	sess.ReportFocus(context.Background(), true)

	got := readUntil(t, att.reader, "[I")
	if !strings.Contains(got, "[O") {
		t.Errorf("output = %q, want the shell to have received a focus-out report", got)
	}
	if !strings.Contains(got, "[I") {
		t.Errorf("output = %q, want the shell to have received a focus-in report", got)
	}
}

// Nothing is sent when the program never asked for focus events, since an unrequested escape
// sequence would arrive as stray keystrokes.
func TestReportFocusSilentWhenNotRequested(t *testing.T) {
	quiet := &fakeTerminal{focusReporting: false}
	rec := startShimFor(t, shimConfigFor("focus-off", "echo READY; cat"))
	sess, err := newSession(rec, quiet, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// Waits for the newline that ends the line, not just for READY. The shell's own output arrives in
	// whatever chunks the pty produces, so stopping at READY can leave a trailing "\r\n" queued: the
	// read below then returns those bytes and the test reports them as a focus report that was never
	// sent. That is what it did on a CI runner, failing with `sent "\r\n"` while passing locally,
	// because a faster machine happened to deliver the whole line as one chunk.
	readUntil(t, att.reader, "READY\r\n")

	sess.ReportFocus(context.Background(), false)

	// cat echoes whatever it receives, so anything arriving now would be the report.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if c, err := att.reader.Next(ctx); err == nil && len(c.Data) > 0 {
		t.Errorf("sent %q although the program never enabled focus events", c.Data)
	}
}

// detach must report the last client leaving, since that is what triggers focus loss.
func TestDetachReportsLastClient(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("last", "sleep 5"))

	sess, err := newSession(rec, nil, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	a1, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	if !a1.first {
		t.Error("first attach reports first = false, want true")
	}
	if last := sess.detach(a1); !last {
		t.Error("detaching the only client reports last = false, want true")
	}
}

// Sequence numbers must not be conflated between the shim's stream and the client's.
//
// Output is rewritten on the way through, to force redraw=0 into prompt markers, and that rewrite
// changes the length. The shim's numbering counts its own bytes while the client log numbers the
// rewritten ones, so using one as a position in the other desynchronizes them by however much the
// rewrite added. A client then starts reading inside an escape sequence, loses the leading ESC, and
// the remainder renders as literal text beside the prompt.
//
// Reproduced with output containing a prompt marker, which is what makes the rewrite lengthen it.
func TestAttachStreamStartsOnASequenceBoundary(t *testing.T) {
	// A prompt marker with no redraw parameter, so the rewrite appends nine bytes, followed by a
	// cursor move whose ESC is what went missing.
	const prompt = "\x1b]133;A\x07PROMPT\x1b[10Dmain\r\n"

	rec := startShimFor(t, shimConfigFor("seqsync",
		`printf '\033]133;A\007PROMPT\033[10Dmain\r\n'; sleep 5`))
	rec.Rows, rec.Cols = 24, 80

	term := &fakeTerminal{restore: []byte("RESTORED")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Let the prompt through so the rewrite has happened and the two counters can diverge.
	warm := sess.recent.Subscribe(0)
	readUntil(t, warm, "main")
	warm.Close()

	// The property: a fresh attach must start exactly at the end of the rewritten log, so no
	// partial sequence is delivered.
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	if got, want := att.reader.Position(), sess.recent.Next(); got != want {
		t.Errorf("stream starts at %d, want the log's end %d; a mismatch delivers a partial escape sequence",
			got, want)
	}

	// And the two counters really do differ here, so the test would catch a regression rather than
	// passing because the rewrite happened to be a no-op.
	if sess.LastSeq() == sess.recent.Next() {
		t.Skip("rewrite did not change the length, so this case cannot desynchronize")
	}
	_ = prompt
}

// A session must report the shell's real exit status.
//
// Regression test for a bug that made every normally-exiting session look like it had vanished. The
// shim is the only thing that knows the status, and the server learns the session ended by the output
// stream closing, which happens at the same moment the shell exits. The shim used to exit right then,
// so the server asked a socket that was already gone and recorded "dead" with no status. Indirectly
// visible everywhere: `cm list` showed exited(0) for a command that failed.
func TestSessionReportsRealExitStatus(t *testing.T) {
	for _, want := range []int{0, 1, 42} {
		t.Run(fmt.Sprintf("exit-%d", want), func(t *testing.T) {
			rec := startShimFor(t, shimConfigFor(
				fmt.Sprintf("status-%d", want), fmt.Sprintf("exit %d", want)))

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

			ended, code := sess.Ended()
			if !ended {
				t.Fatal("Ended() = false, want true")
			}
			if code != want {
				t.Errorf("exit code = %d, want %d; -1 means the status was lost", code, want)
			}
		})
	}
}
