package server

import (
	"context"
	"encoding/base64"
	"regexp"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/vt"
)

// A client whose terminal never said it can draw images is sent none.
//
// This is the reported bug: a mobile ssh client attached to a session holding an image and printed the
// payload as base64 across the screen. cm re-transmits stored images to every attaching client, which makes
// cm a sender under the graphics protocol, and a sender asks first. Nothing asked, so nothing knew.
//
// The screen must still arrive, which is the half that makes this a gate rather than a switch: that client
// wants the session, it just cannot draw pictures.
func TestAClientThatCannotDrawImagesGetsNone(t *testing.T) {
	sess, term := sessionWithAnImage(t)
	defer sess.Close()

	// Reserved the way the service reserves, and left saying nothing, which is what a terminal that failed
	// the probe and a client predating it both report.
	quiet, err := sess.attach(nil, sess.reserveClient())
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(quiet)
	got := string(quiet.restore)

	if strings.Contains(got, "a=t") || strings.Contains(got, "a=T") {
		t.Errorf("the restore carries an image transmission; restore = %q", truncate(got))
	}
	if strings.Contains(got, "a=p") {
		t.Errorf("the restore carries a placement, which draws nothing and names an image the terminal was "+
			"never sent; restore = %q", truncate(got))
	}
	// The payload itself, checked separately: this is the thing that appeared as text on the user's screen.
	if strings.Contains(got, imagePayload) {
		t.Errorf("the restore carries the image payload, which a terminal that cannot draw it prints as "+
			"text; restore = %q", truncate(got))
	}
	// And it is still a working attach rather than a punishment.
	if !strings.Contains(got, "before the image") {
		t.Errorf("the restore lost the screen as well as the images; restore = %q", truncate(got))
	}

	// The same session, a terminal that did answer: proves the gate is what differs and not the fixture.
	drawing, err := sess.attach(nil, drawingClient(t, sess))
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(drawing)
	if drew := string(drawing.restore); !strings.Contains(drew, "a=t") || !strings.Contains(drew, "a=p") {
		t.Errorf("a terminal that answered yes got no image, so the gate is stuck shut; restore = %q",
			truncate(drew))
	}
	_ = term
}

// imagePayload is the base64 of the image the session below displays, which is what a terminal that cannot
// draw it would print.
const imagePayload = "AQIDAQIDAQIDAQID"

// sessionWithAnImage returns a session whose screen holds one image, fed through the pump so the store and
// the model agree the way they do in a real session.
func sessionWithAnImage(t *testing.T) (*Session, *vt.SessionTerminal) {
	t.Helper()
	rec := startShimFor(t, shimConfigFor("gfxgate", "sleep 10"))
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	if err := sess.Resize(context.Background(), 24, 80, 800, 480); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// Two by two at 24 bits, so the payload is exactly the twelve bytes the geometry implies.
	raw := []byte{1, 2, 3, 1, 2, 3, 1, 2, 3, 1, 2, 3}
	if got := base64.StdEncoding.EncodeToString(raw); got != imagePayload {
		t.Fatalf("fixture payload is %q, want %q", got, imagePayload)
	}
	feed := func(chunk string) {
		before := sess.recent.Next()
		out := feedGraphics(t, sess, chunk)
		sess.feedTerminal([]byte(out), before+seq.Log(len(out)))
	}
	feed("before the image\r\n")
	feed("\x1b_Ga=T,q=2,f=24,s=2,v=2,i=7;" + imagePayload + "\x1b\\")
	return sess, term
}

// The late send carries both halves, and marks the attachment so a later restore does too.
//
// This is what a terminal that answers after its attach receives. It has already been served a screen with
// no images, so these bytes have to stand alone: transmissions to make the images exist again, placements to
// put them where the screen says they are. Either half alone draws nothing.
func TestImagesForALateAnswerCarryTransmissionsAndPlacements(t *testing.T) {
	sess, _ := sessionWithAnImage(t)
	defer sess.Close()

	tok := sess.reserveClient()
	got := string(sess.imagesFor(tok))

	if !strings.Contains(got, "a=t") {
		t.Errorf("no transmission, so the terminal has no image to place; got %q", truncate(got))
	}
	if !strings.Contains(got, imagePayload) {
		t.Errorf("no payload, so the transmission is empty; got %q", truncate(got))
	}
	if !strings.Contains(got, "a=p") {
		t.Errorf("no placement, so the payload draws nothing; got %q", truncate(got))
	}
	// Order, which is the same rule the inline restore follows: a placement naming an image the terminal has
	// not been sent is discarded.
	if i, j := strings.Index(got, "a=t"), strings.Index(got, "a=p"); i > j {
		t.Errorf("placement precedes its transmission, so the terminal drops it; got %q", truncate(got))
	}
	if !tok.drawsImages {
		t.Error("the token was not marked, so a later restore for this same client would carry no images")
	}
	// And a restore taken after this does carry them inline, which is what that mark is for.
	att, err := sess.attach(nil, tok)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)
	if inline := string(att.restore); !strings.Contains(inline, imagePayload) {
		t.Errorf("a restore after a late answer lost the images; restore = %q", truncate(inline))
	}
}

// Nothing to send when the session holds no image, which is nearly every attach: the answer arrives whether
// or not there is anything to say. Sending an empty push would cost a round trip per attach.
func TestImagesForASessionWithNoImagesIsEmpty(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("gfxnone", "sleep 10"))
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	if got := sess.imagesFor(sess.reserveClient()); got != nil {
		t.Errorf("imagesFor() = %q, want nil for a session with no images", got)
	}
}

// Late images change nothing that is already on screen.
//
// This is the property the whole no-wait design rests on, and it is worth an emulator rather than an argument:
// the client has already painted its screen when these bytes arrive, so if they moved the cursor or printed
// anything, a picture would cost a corrupted screen. They do not, for two reasons that are both cm's to keep.
// The commands paint no text, and the placements are wrapped in DECSC/DECRC so the cursor ends where it
// started.
//
// The space an image occupies is already reserved by the time this runs, which is why nothing has to reflow: a
// program that draws an image moves the cursor past it, so the text layout counts those rows and the cells
// themselves hold no characters.
func TestLateImagesDoNotDisturbThePaintedScreen(t *testing.T) {
	sess, _ := sessionWithAnImage(t)
	defer sess.Close()

	// The screen a client that said nothing was served: text, no images. This is what it has on screen when
	// the late bytes arrive.
	quiet, err := sess.attach(nil, sess.reserveClient())
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(quiet)

	// An emulator standing in for that client's terminal, so the assertion is what a terminal does with these
	// bytes rather than what they look like.
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer term.Close()
	if err := term.Resize(24, 80, 10, 20); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if err := term.Write(quiet.restore); err != nil {
		t.Fatalf("Write(restore) error = %v", err)
	}
	beforeText, err := term.Plain()
	if err != nil {
		t.Fatalf("Plain() error = %v", err)
	}
	beforeVT, err := term.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	late := sess.imagesFor(sess.reserveClient())
	if len(late) == 0 {
		t.Fatal("no late images to send, so this test proves nothing")
	}
	if err := term.Write(late); err != nil {
		t.Fatalf("Write(late) error = %v", err)
	}

	afterText, err := term.Plain()
	if err != nil {
		t.Fatalf("Plain() error = %v", err)
	}
	if got, want := string(afterText), string(beforeText); got != want {
		t.Errorf("the text on screen changed when the images arrived.\n got: %q\nwant: %q", got, want)
	}
	afterVT, err := term.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if got, want := lastCursorPosition(afterVT), lastCursorPosition(beforeVT); got != want {
		t.Errorf("the cursor moved to %q from %q: a placement is positioned with CUP, so it has to be "+
			"wrapped in DECSC/DECRC or the next thing the program prints lands on the image", got, want)
	}
}

// cupPattern matches a CUP, which is how a serialized screen states where the cursor ended.
var cupPattern = regexp.MustCompile(`\x1b\[([0-9]+);([0-9]+)H`)

// lastCursorPosition reports where a serialized screen leaves the cursor.
func lastCursorPosition(vt []byte) string {
	all := cupPattern.FindAllString(string(vt), -1)
	if len(all) == 0 {
		return ""
	}
	return all[len(all)-1]
}
