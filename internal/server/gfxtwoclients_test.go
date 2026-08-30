package server

import (
	"bytes"
	"compress/zlib"
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

	// 20x20 RGB, chunked the way icat chunks: control keys and payload on the first, payload alone after.
	var px []byte
	for i := 0; i < 20*20; i++ {
		px = append(px, 1, 2, 3)
	}
	// Compressed, because icat compresses: the captured command is a=T,q=2,f=24,o=z,m=1,s=800,v=503.
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	zw.Write(px)
	zw.Close()
	enc := kittyEnc(zbuf.Bytes())
	// At a multiple of four, which is what a real client does: base64 chunks are concatenated by the
	// receiver, so a split inside a quantum would corrupt the image.
	half := len(enc) / 2 / 4 * 4
	feed := func(chunk string) {
		before := sess.recent.Next()
		out := feedGraphics(t, sess, chunk)
		sess.feedTerminal([]byte(out), before+seq.Log(len(out)))
	}
	feed("\x1b_Ga=T,q=2,f=24,o=z,m=1,s=20,v=20;" + enc[:half] + "\x1b\\")
	feed("\x1b_Ga=T,q=2,m=0;" + enc[half:] + "\x1b\\")

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
