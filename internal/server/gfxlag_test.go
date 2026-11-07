package server

import (
	"testing"
	"testing/synctest"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/vt"
)

// A terminal that answers the graphics probe while the model is behind the log still gets the image.
//
// The client loop strips images from a client that has not answered yet, and the pump feeds the model only
// after clients have the chunk. An answer landing between the two found the transmission in cm's store and
// no placement in the model, so imagesFor sent nothing, and the client never received the image: not in the
// stream, which had been stripped, and not in the push. Found while chasing TestImagesSurviveAServerRestart,
// though that flake turned out to have other causes.
//
// synctest rather than a fault, because the two orderings need opposite things from the pump: the broken
// one has to read placements while the model lags, and the fixed one has to wait for the model to move.
// synctest.Wait returns once imagesFor has either returned or blocked, so the model is fed only after that,
// whichever code is under test.
func TestALateAnswerDuringModelLagStillGetsTheImage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		term, err := vt.NewSessionTerminal(24, 80, 0)
		if err != nil {
			t.Fatalf("NewSessionTerminal() error = %v", err)
		}
		sess := &Session{
			id:            "gfxlag",
			label:         "gfxlag",
			recent:        seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
			term:          term,
			gfxStore:      graphics.NewStore(0),
			gfxCellHeight: 20,
			log:           cmlog.Discard(),
			clientSizes:   make(map[*attachToken]*clientSize),
			evicts:        make(map[*attachToken]chan struct{}),
			queries:       make(map[*attachToken]chan []byte),
			done:          make(chan struct{}),
		}
		defer term.Close()
		// A cell size, as a client's first resize provides. Without one the model has no pixel scale to place
		// an image on, and reports no placements at all.
		if err := term.Resize(24, 80, 10, 20); err != nil {
			t.Fatalf("Resize() error = %v", err)
		}

		// What the pump does before clients see a chunk: the store has the transmission and the log has the
		// chunk. The model has not been fed it yet.
		out := feedGraphics(t, sess, "\x1b_Ga=T,q=2,f=24,s=2,v=2,i=7;"+imagePayload+"\x1b\\")

		var got []byte
		answered := make(chan struct{})
		go func() {
			got = sess.imagesFor(sess.reserveClient())
			close(answered)
		}()
		synctest.Wait()

		// The pump catches up.
		sess.feedTerminal([]byte(out), sess.recent.Next())
		<-answered

		// The same push a client answering now would get, with the model current.
		want := sess.imagesFor(sess.reserveClient())
		if len(want) == 0 {
			t.Fatal("imagesFor() with the model current is empty, so the fixture holds no image to compare against")
		}
		if string(got) != string(want) {
			t.Errorf("imagesFor() answered during model lag = %q, want %q.\n"+
				"The client loop had already stripped the image from this client's stream, so this push is "+
				"the only way it gets the picture.", truncate(string(got)), truncate(string(want)))
		}
	})
}
