package server

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/vt"
)

// replayTerminal is a fake emulator that records what it was fed, so replay can be tested without
// the cgo terminal.
func replayTerminal(t *testing.T) (NewTerminalFunc, *fakeTerminal) {
	t.Helper()
	term := &fakeTerminal{restore: []byte("REPLAYED_SCREEN")}
	return func(rows, cols uint16) (Terminal, error) {
		term.Resize(rows, cols, 0, 0)
		return term, nil
	}, term
}

// seedLog writes a persisted log holding content and returns its path.
func seedLog(t *testing.T, name, content string, limits seqlog.FileLimits) string {
	t.Helper()
	path := filepath.Join(shortTempDir(t), name)
	f, err := seqlog.OpenFile[seq.Shim](path, limits)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()
	if content != "" {
		if err := f.Append([]byte(content)); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	return path
}

// The property reboot persistence rests on: a saved log becomes a screen in the session's own model.
func TestSeedFromPersistedLogRebuildsScreen(t *testing.T) {
	path := seedLog(t, "session.log", "line one\r\nline two\r\n", seqlog.FileLimits{})

	term := &fakeTerminal{restore: []byte("REPLAYED_SCREEN")}
	if err := seedFromPersistedLog(path, term, nil, seqlog.FileLimits{}); err != nil {
		t.Fatalf("seedFromPersistedLog() error = %v", err)
	}

	if got, want := term.Written(), "line one\r\nline two\r\n"; got != want {
		t.Errorf("emulator saw %q, want %q", got, want)
	}
}

// Query responses generated during replay must be discarded. They answer questions a program asked
// before the reboot, and that program is gone; delivering them would inject stray input into a new
// shell.
func TestSeedFromPersistedLogDiscardsGeneratedInput(t *testing.T) {
	path := seedLog(t, "session.log", "content\r\n", seqlog.FileLimits{})

	term := &fakeTerminal{
		restore: []byte("SCREEN"),
		pending: [][]byte{[]byte("\x1b[0n")},
	}
	if err := seedFromPersistedLog(path, term, nil, seqlog.FileLimits{}); err != nil {
		t.Fatalf("seedFromPersistedLog() error = %v", err)
	}
	if len(term.TakePending()) != 0 {
		t.Error("replay left generated input queued, which would reach a new shell as keystrokes")
	}
}

func TestSeedFromPersistedLogMissingCases(t *testing.T) {
	dir := shortTempDir(t)
	term := &fakeTerminal{restore: []byte("SCREEN")}

	tests := []struct {
		name string
		path string
		term Terminal
	}{
		{"no path", "", term},
		{"missing file", filepath.Join(dir, "absent.log"), term},
		// Without an emulator the bytes cannot become a screen, and replaying them raw would dump
		// the whole log into the terminal.
		{"no terminal", filepath.Join(dir, "absent.log"), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := seedFromPersistedLog(tt.path, tt.term, nil, seqlog.FileLimits{})
			if !errors.Is(err, ErrNothingToRestore) {
				t.Errorf("error = %v, want ErrNothingToRestore", err)
			}
		})
	}
}

// An empty log is nothing to restore rather than an error, since a session that persisted but
// produced no output is a normal thing to encounter.
func TestSeedFromPersistedLogEmptyLog(t *testing.T) {
	path := seedLog(t, "empty.log", "", seqlog.FileLimits{})

	term := &fakeTerminal{restore: []byte("SCREEN")}
	err := seedFromPersistedLog(path, term, nil, seqlog.FileLimits{})
	if !errors.Is(err, ErrNothingToRestore) {
		t.Errorf("error = %v, want ErrNothingToRestore for an empty log", err)
	}
}

// A trimmed log still replays, and only its retained tail reaches the model. The log's own numbering
// is deliberately not carried across: those positions belong to a dead incarnation, and the new shim
// numbers from zero.
func TestSeedFromPersistedLogAfterTrim(t *testing.T) {
	limits := seqlog.FileLimits{MaxLines: 2}
	path := filepath.Join(shortTempDir(t), "trimmed.log")
	f, err := seqlog.OpenFile[seq.Shim](path, limits)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	for _, line := range []string{"dropped\r\n", "kept one\r\n", "kept two\r\n"} {
		if err := f.Append([]byte(line)); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	oldest, _ := f.Bounds()
	f.Close()

	if oldest == 0 {
		t.Fatal("log was not trimmed, so this test would not exercise the case")
	}

	term := &fakeTerminal{restore: []byte("SCREEN")}
	if err := seedFromPersistedLog(path, term, nil, limits); err != nil {
		t.Fatalf("seedFromPersistedLog() error = %v", err)
	}
	if got, want := term.Written(), "kept one\r\nkept two\r\n"; got != want {
		t.Errorf("emulator saw %q, want only the retained tail %q", got, want)
	}
}

// The images in a persisted log must land in the session's graphics store, or the placements on the
// replayed screen name ids the client's terminal has never received and draw nothing.
func TestSeedFromPersistedLogRecordsImages(t *testing.T) {
	// What cm writes to its log: the command already inlined and already named, plus ordinary output.
	path := seedLog(t, "session.log",
		"before\r\n\x1b_Ga=T,f=24,s=2,v=2,i=7;QUJDQUJDQUJDQUJD\x1b\\after\r\n", seqlog.FileLimits{})

	term := &fakeTerminal{restore: []byte("SCREEN")}
	gfx := graphics.NewStore(0)
	if err := seedFromPersistedLog(path, term, gfx, seqlog.FileLimits{}); err != nil {
		t.Fatalf("seedFromPersistedLog() error = %v", err)
	}

	rt := gfx.Retransmissions()
	if len(rt) != 1 {
		t.Fatalf("store holds %d retransmissions, want the one image in the log: %+v", len(rt), rt)
	}
	if rt[0].ID != 7 || rt[0].ByNumber {
		t.Errorf("retransmission identifies {ID:%d ByNumber:%v}, want {ID:7 ByNumber:false}",
			rt[0].ID, rt[0].ByNumber)
	}
	got := string(rt[0].Bytes)
	if !strings.Contains(got, "QUJDQUJDQUJDQUJD") {
		t.Errorf("the retransmission carries no payload; got %q", got)
	}
	// Stored without displaying, or the image is drawn wherever the cursor happens to be and then
	// erased by the screen that follows it.
	if strings.Contains(got, "a=T") {
		t.Errorf("the retransmission displays at the cursor; got %q", got)
	}
}

// The whole of reboot recovery at the session level: a revived session serves its pre-reboot screen
// and its images to *every* client, from its own model, through the ordinary restore path.
//
// This is what the earlier shape could not do. Persistence used to replay into a throwaway terminal,
// serialize it, and hand the blob to the first client to attach, so a second client saw a session with
// no history and the images bypassed the transmit-then-place split the ordinary path performs. The
// order is the assertion, because each part is useless in the wrong place: a transmission after the
// screen is an image the placement cannot resolve, and a placement before it is one the screen's own
// clear erases.
func TestRevivedSessionServesEveryClientFromItsModel(t *testing.T) {
	// Two cells wide and one tall at 10x20 metrics, drawn on the second row.
	var payload []byte
	for i := 0; i < 20*20; i++ {
		payload = append(payload, 1, 2, 3)
	}
	cmd := "\x1b_Ga=T,f=24,s=20,v=20,i=7;" + base64.StdEncoding.EncodeToString(payload) + "\x1b\\"
	path := seedLog(t, "session.log", "OUTPUT_FROM_BEFORE\r\n"+cmd, seqlog.FileLimits{})

	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	gfx := graphics.NewStore(0)
	if err := seedFromPersistedLog(path, term, gfx, seqlog.FileLimits{}); err != nil {
		t.Fatalf("seedFromPersistedLog() error = %v", err)
	}

	rec := startShimFor(t, shimConfigFor("revived", "sleep 5"))
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()
	sess.setGraphicsStore(gfx)

	// The cell metrics a client's attach carries, without which every placement reports itself
	// off-screen and the images are silently dropped from the restore.
	if err := sess.Resize(context.Background(), 24, 80, 800, 480); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	for _, which := range []string{"first", "second"} {
		att, err := sess.attach(nil, nil)
		if err != nil {
			t.Fatalf("%s attach() error = %v", which, err)
		}
		got := string(att.restore)
		sess.detach(att)

		if !strings.Contains(got, "OUTPUT_FROM_BEFORE") {
			t.Errorf("%s client got no pre-reboot content; restore = %q", which, truncate(got))
		}
		transmit := strings.Index(got, "a=t")
		place := strings.Index(got, "a=p,i=7")
		if transmit < 0 || place < 0 {
			t.Fatalf("%s client is missing a piece: transmit=%d place=%d in %q",
				which, transmit, place, truncate(got))
		}
		if transmit > place {
			t.Errorf("%s client got the transmission after the placement, so the id cannot resolve",
				which)
		}
		// Positioned where it was drawn, on the second row, one-based in CUP.
		if !strings.Contains(got, "\x1b[2;1H") {
			t.Errorf("%s client's placement is not at row 1 column 0; restore = %q", which, truncate(got))
		}
	}
}
