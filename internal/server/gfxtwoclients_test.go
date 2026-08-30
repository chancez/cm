package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/vt"
)

// kittyEnc encodes the way a graphics client does: unpadded.
func kittyEnc(b []byte) string {
	return base64.RawStdEncoding.EncodeToString(b)
}

// Reported: with two clients attached, or a new client attaching after an image was drawn, the second
// client shows no image. Driven the way icat drives it: no i= at all, chunked with m=, a=T.
func TestTwoClientsBothGetTheLiveImage(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("gfxtwo", "sleep 10"))
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()
	if err := sess.Resize(context.Background(), 24, 80, 800, 480); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// The first client, attached before the image arrives, as in the report.
	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	defer sess.detach(first)

	// Sized like a real screenshot and chunked at 4096 the way kitty's client chunks. Scale is the whole
	// point: a small image goes out in one command and a real one in hundreds, and only the small case
	// was ever tested.
	const iw, ih = 800, 600
	px := make([]byte, iw*ih*3)
	for i := range px {
		px[i] = byte(i % 251)
	}
	enc := kittyEnc(px)
	feed := func(chunk string) {
		before := sess.recent.Next()
		out := feedGraphics(t, sess, chunk)
		sess.feedTerminal([]byte(out), before+seq.Log(len(out)))
	}
	const step = 4096
	firstChunk := true
	chunks := 0
	for i := 0; i < len(enc); i += step {
		end := min(i+step, len(enc))
		more := "1"
		if end == len(enc) {
			more = "0"
		}
		control := "a=T,q=2,m=" + more
		if firstChunk {
			control = "a=T,q=2,f=24,s=800,v=600,i=7,m=" + more
			firstChunk = false
		}
		feed("\x1b_G" + control + ";" + enc[i:end] + "\x1b\\")
		chunks++
	}
	t.Logf("fed %d chunks, %d bytes of base64", chunks, len(enc))

	// What the model and the store each hold, reported before the assertion so a failure says which
	// half is missing rather than only that the restore is wrong.
	places, perr := term.Placements()
	t.Logf("model placements = %+v (err %v)", places, perr)
	t.Logf("store retransmissions = %d", len(sess.gfxStore.Retransmissions()))

	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(second)
	got := string(second.restore)

	if !strings.Contains(got, "a=t") {
		t.Fatalf("the second client got no transmission; restore = %q", truncate(got))
	}
	if !strings.Contains(got, "a=p") {
		t.Fatalf("the second client got no placement; restore = %q", truncate(got))
	}

	// The assertion that matters, and the one presence checks cannot make: feed the restore to a fresh
	// emulator, the way the second client's terminal receives it, and ask whether an image ends up on the
	// screen. A restore can carry every byte and still draw nothing, which is what the user sees.
	fresh, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer fresh.Close()
	if err := fresh.Resize(24, 80, 10, 20); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if err := fresh.Write(second.restore); err != nil {
		t.Fatalf("replaying the restore error = %v", err)
	}
	drawn, err := fresh.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	if len(drawn) == 0 {
		t.Errorf("the restore drew no image in a fresh terminal, so the second client shows nothing; "+
			"restore = %q", truncate(got))
	}
}
