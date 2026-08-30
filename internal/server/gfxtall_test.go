package server

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/vt"
)

// A tall image, scrolled so its top is above the viewport, must still draw for a second client.
//
// This is the shape of every real image rather than an edge case: `kitten icat` scales to nearly fill the
// window, and the prompt printed under it scrolls the top off, so the placement sits at a negative row.
// A small fixture never reaches that path, which is why it needs its own test.
func TestATallScrolledImageStillDrawsForASecondClient(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("gfxtall", "sleep 10"))
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()
	// 24 rows of 20px cells, so the viewport is 480px tall.
	if err := sess.Resize(context.Background(), 24, 80, 800, 480); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	first, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("first attach() error = %v", err)
	}
	defer sess.detach(first)

	// 440px tall, 22 of the 24 rows, which is what icat aims for.
	const w, h = 100, 440
	var px []byte
	for i := 0; i < w*h; i++ {
		px = append(px, 1, 2, 3)
	}
	cmd := "\x1b_Ga=T,q=2,f=24,s=100,v=440,i=7;" + base64.RawStdEncoding.EncodeToString(px) + "\x1b\\"

	feed := func(chunk string) {
		before := sess.recent.Next()
		out := feedGraphics(t, sess, chunk)
		sess.feedTerminal([]byte(out), before+seq.Log(len(out)))
	}
	feed(cmd)
	// The prompt underneath, which scrolls the image's top above the viewport. Nine lines, which measured
	// as row -7 on the reported case.
	feed(strings.Repeat("prompt\r\n", 9))

	places, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	t.Logf("model placements = %+v", places)
	if len(places) == 0 {
		t.Fatal("the model reports no placement, so the fixture never put an image on screen")
	}
	if places[0].Row >= 0 {
		t.Fatalf("placement is at row %d, not scrolled above the viewport: this test would not exercise "+
			"the crop path", places[0].Row)
	}

	second, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(second)

	// The oracle: replay what the second client received into a fresh model and ask whether an image ends
	// up on its screen.
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
		t.Errorf("the restore drew no image in a fresh terminal, so the second client shows a blank space "+
			"where the picture is. restore = %q", truncate(string(second.restore)))
	}
}
