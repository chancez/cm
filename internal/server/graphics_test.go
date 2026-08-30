package server

import (
	"encoding/base64"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/shim"
)

// A transmission naming a temp file must not reach the client, and its data must.
//
// This is the reported bug at the seam that produced it. A transfer file is consumed once: kitty opens
// the path, reads it, and unlinks it, so forwarding the command let the program and the real terminal
// race for it. Two of icat's three probes name a path and both came back
// "EBADF ... No such file or directory" while the inline one answered OK.
func TestGraphicsFileTransferIsInlined(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxfile",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// A file named the way a graphics client names one, so cm will read it.
	f, err := os.CreateTemp("", "kitty-tty-graphics-protocol-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	defer os.Remove(f.Name())
	const pixels = "\x01\x02\x03"
	if _, err := f.WriteString(pixels); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	f.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	cmd := "\x1b_Ga=T,f=24,t=t,s=1,v=1,i=1;" +
		base64.StdEncoding.EncodeToString([]byte(f.Name())) + "\x1b\\"
	got := feedGraphics(t, sess, cmd)

	// The path must be gone from what the client sees, or the terminal opens a file cm already read.
	if strings.Contains(got, f.Name()) {
		t.Errorf("the client received the transfer path; stream = %q", got)
	}
	if strings.Contains(got, "t=t") {
		t.Errorf("the client received a file medium; stream = %q", got)
	}
	// And the data has to be there instead, or the image never draws.
	wantPayload := base64.StdEncoding.EncodeToString([]byte(pixels))
	if !strings.Contains(got, wantPayload) {
		t.Errorf("the inlined payload %q is missing; stream = %q", wantPayload, got)
	}

	// cm consumed the temp file, so it must be gone: leaving it accumulates one per image, since the
	// program hands over the path and never looks again.
	if _, err := os.Stat(f.Name()); !os.IsNotExist(err) {
		t.Errorf("the temp transfer file survived (stat err = %v)", err)
	}
}

// The interception has to be wired into the output pump, not merely implemented.
//
// This test exists because the others do not catch that. They drive handleGraphics directly, which is
// the right seam for what the interception *produces*, and every one of them still passed with the call
// removed from the pump: mutation-tested by deleting it and watching them stay green.
//
// The program's output is produced by the shell rather than written to the pty, and that distinction is
// the whole reason this works. Writing the command with sess.Write sends it as *input*, and a shell at a
// prompt echoes input back in caret notation: the first attempt at this test asserted on a stream
// containing a literal "^[_Ga=T..." with `^` and `[` as separate printable bytes, so the scanner had
// nothing to find and the test failed while the code was correct. `docs/testing.md` names that trap, and
// this is what it looks like from the inside. printf in the shell emits real escape bytes on the output
// path instead.
func TestGraphicsInterceptionIsWiredIntoThePump(t *testing.T) {
	f, err := os.CreateTemp("", "kitty-tty-graphics-protocol-*")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("\x01\x02\x03"); err != nil {
		t.Fatalf("WriteString() error = %v", err)
	}
	f.Close()

	// The shell emits the command as output, exactly as a graphics client would. Octal escapes rather
	// than \e, since /bin/sh on Linux is dash and does not accept it.
	path := base64.StdEncoding.EncodeToString([]byte(f.Name()))
	script := "printf '\\033_Ga=T,f=24,t=t,s=1,v=1,i=1;" + path + "\\033\\\\'; sleep 10"

	rec := startShimFor(t, shim.Config{
		Session: "gfxpump",
		Command: []string{"/bin/sh", "-c", script},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// The inlined payload proves the pump ran the interception: those bytes are in the file, never on the
	// wire, so only cm reading it can have put them in the stream.
	wantPayload := base64.StdEncoding.EncodeToString([]byte("\x01\x02\x03"))
	got := awaitStream(t, sess, wantPayload, 5*time.Second)
	if !strings.Contains(got, wantPayload) {
		t.Errorf("the pump did not intercept the transfer: stream = %q.\n"+
			"The scanner and handleGraphics are tested directly elsewhere, so this failing while those "+
			"pass means the call is missing from the pump rather than the logic being wrong.", got)
	}
	if strings.Contains(got, f.Name()) {
		t.Errorf("the transfer path reached the client; stream = %q", got)
	}
}

// An inline transmission passes through, since there is nothing to resolve and no race to remove.
func TestGraphicsInlineTransmissionReachesTheClient(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxinline",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	got := feedGraphics(t, sess, "\x1b_Ga=T,f=24,s=1,v=1,i=1;AQID\x1b\\")
	if !strings.Contains(got, "AQID") {
		t.Errorf("an inline transmission did not reach the client; stream = %q", got)
	}
}

// The images a session transmitted are re-sent when a client attaches, which is what makes them survive
// a reattach. Before this they were absent, because libghostty's formatter does not re-emit them.
func TestGraphicsImagesAreResentOnAttach(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxresend",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("SCREEN")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Transmitted with no client attached, which is the case that matters: the image has to come from
	// cm's store rather than from anything a client saw.
	sess.handleGraphics(parseAll(t, "\x1b_Ga=T,f=24,s=1,v=1,i=42;AQID\x1b\\"))

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	restore := string(att.restore)
	if !strings.Contains(restore, "AQID") {
		t.Errorf("the restore blob has no image payload; got %q", restore)
	}
	if !strings.Contains(restore, "i=42") {
		t.Errorf("the restore blob does not identify the image; got %q", restore)
	}

	// Images must come before the screen, or a placement referring to one draws nothing because the
	// terminal has never seen that id.
	img := strings.Index(restore, "AQID")
	screen := strings.Index(restore, "SCREEN")
	if img < 0 || screen < 0 || img > screen {
		t.Errorf("images must precede the screen: image at %d, screen at %d in %q",
			img, screen, restore)
	}

	// And the re-transmission must be quiet, or the client's terminal answers a command cm sent and the
	// reply arrives on the input path answering a question cm never asked.
	if !strings.Contains(restore, "q=2") {
		t.Errorf("the re-transmission is not quiet; got %q", restore)
	}
}

// A transfer cm refuses is dropped rather than forwarded, since forwarding puts the program and the
// terminal back in the race. The program has a fallback: icat negotiates stream and exits 0.
func TestGraphicsRefusedTransferIsNotForwarded(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxrefuse",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// A path outside any temp directory, which cm will not read.
	cmd := "\x1b_Ga=q,f=24,t=t,s=1,v=1,i=9;" +
		base64.StdEncoding.EncodeToString([]byte("/etc/passwd")) + "\x1b\\"
	got := feedGraphics(t, sess, cmd)

	if strings.Contains(got, "passwd") || strings.Contains(got, "t=t") {
		t.Errorf("a refused transfer was forwarded; stream = %q", got)
	}
}

// Ordinary output around a command is preserved, since a session's text must not be disturbed by the
// bytes cm removes from between it.
func TestGraphicsPreservesSurroundingOutput(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxaround",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	got := feedGraphics(t, sess, "BEFORE\x1b_Ga=T,f=24,s=1,v=1,i=1;AQID\x1b\\AFTER")
	if !strings.Contains(got, "BEFORE") || !strings.Contains(got, "AFTER") {
		t.Errorf("output around the command was lost; stream = %q", got)
	}
}

// feedGraphics runs a chunk through the interception the pump uses and returns what a client would see.
//
// Drives handleGraphics and the scanner rather than the whole pump, because the pump reads from a shim
// and this is about what the interception produces. The bytes are also appended to the session's log so
// an attached client's stream can be read back, which is what an end-to-end assertion needs.
func feedGraphics(t *testing.T, sess *Session, chunk string) string {
	t.Helper()
	segs := sess.gfxScan.Scan([]byte(chunk))
	out := []byte(chunk)
	if segs != nil {
		out = sess.handleGraphics(segs)
	}
	sess.recent.Append(out)
	return string(out)
}

// parseAll parses every command in a chunk, for a test that wants them without the surrounding bytes.
func parseAll(t *testing.T, chunk string) []graphics.Segment {
	t.Helper()
	var sc graphics.Scanner
	segs := sc.Scan([]byte(chunk))
	if len(segs) == 0 {
		t.Fatalf("no graphics commands parsed from %q", chunk)
	}
	return segs
}

// The restore transmits an image without displaying it and places it after the screen, and both halves
// of that ordering were bugs.
//
// cm used to re-emit a stored a=T, transmit *and display at the cursor*, ahead of the screen blob. Two
// things went wrong, both measured in a real kitty. The blob begins by clearing, and a clear deletes the
// placements on the cells it erases, so the image was drawn and then wiped: a client attaching to a
// session with an image on screen saw no image. And where a=T draws is wherever the cursor is when the
// bytes land, so with a full-screen program running the same restore drew the image on top of the
// program, which is the reported picture sitting over fzf.
//
// So: transmission first and a=t, placement last and a=p.
func TestGraphicsRestorePlacesImagesAfterTheScreen(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxplace",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	term := &fakeTerminal{
		restore: []byte("SCREEN"),
		// What the model reports: image 42 sits at row 5, column 2, covering 4x3 cells.
		placements: []graphics.Placement{{ImageID: 42, Col: 2, Row: 5, Columns: 4, Rows: 3}},
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sess.handleGraphics(parseAll(t, "\x1b_Ga=T,f=24,s=1,v=1,i=42;AQID\x1b\\"))

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	restore := string(att.restore)

	// The transmission must not display, or it draws at whatever the cursor is.
	if strings.Contains(restore, "a=T") {
		t.Errorf("the restore re-transmits with a=T, which displays at the cursor; got %q", restore)
	}
	if !strings.Contains(restore, "a=t") {
		t.Errorf("the restore does not transmit the image; got %q", restore)
	}

	// And the placement has to follow the screen, or the screen's clear erases it.
	payload := strings.Index(restore, "AQID")
	screen := strings.Index(restore, "SCREEN")
	place := strings.Index(restore, "a=p")
	if payload < 0 || screen < 0 || place < 0 {
		t.Fatalf("restore is missing a piece: payload=%d screen=%d placement=%d in %q",
			payload, screen, place, restore)
	}
	if !(payload < screen && screen < place) {
		t.Errorf("restore order is payload=%d screen=%d placement=%d, want payload < screen < placement "+
			"in %q", payload, screen, place, restore)
	}
	// At the position the model reported, one-based.
	if !strings.Contains(restore, "\x1b[6;3H") {
		t.Errorf("the placement is not positioned at row 5 column 2; got %q", restore)
	}
}

// A model that cannot report its placements costs the pictures, not the attach: a client with a correct
// screen and no images is far better than a client that cannot connect.
func TestGraphicsRestoreSurvivesAPlacementReadFailure(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "gfxplacefail",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	term := &fakeTerminal{
		restore:       []byte("SCREEN"),
		placementsErr: errors.New("placements unavailable"),
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sess.handleGraphics(parseAll(t, "\x1b_Ga=T,f=24,s=1,v=1,i=42;AQID\x1b\\"))

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v, want the attach to succeed without placements", err)
	}
	defer sess.detach(att)

	restore := string(att.restore)
	if !strings.Contains(restore, "SCREEN") {
		t.Errorf("the screen is missing from the restore; got %q", restore)
	}
	if strings.Contains(restore, "a=p") {
		t.Errorf("placements were emitted despite the read failing; got %q", restore)
	}
}
