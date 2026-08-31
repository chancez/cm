package server

import (
	"testing"
	"time"
)

// graphicsQuery is what `kitten icat` asks the terminal before sending anything, and what cm's own probe
// looks like: an APC asking whether a one-pixel transmission would work.
const graphicsQuery = "\x1b_Ga=q,i=31,s=1,v=1,t=d,f=24;AAAA\x1b\\"

// A graphics query is never put to a terminal that cannot draw images.
//
// The reported bug. A kitty and a mobile ssh client were attached to one session and `icat` ran in the kitty.
// The proxy asks the most recently attached client, which was the phone, and a terminal with no graphics
// support cannot parse an APC: it printed "Ga=q,f=24,s=1,v=1" across the screen and answered nothing. Gating
// the output stream did not cover it, because a proxied query does not travel in the output stream.
func TestAGraphicsQueryIsNotPutToATerminalThatCannotDrawImages(t *testing.T) {
	sess, att := sessionWithQuietClient(t)
	defer sess.detach(att)

	sess.processChunk([]byte("painting"+graphicsQuery+"and carrying on"), 0)

	if got := outstandingRequests(sess); got != 0 {
		t.Errorf("outstanding proxied requests = %d, want 0: this client's terminal cannot answer a graphics "+
			"query and prints it as text", got)
	}
	select {
	case q := <-att.queries:
		t.Errorf("the client was asked %q, which its terminal renders as text", q)
	default:
	}
}

// The same query does go to a terminal that can draw them, which is what makes the test above about
// eligibility rather than about graphics queries being dropped altogether.
func TestAGraphicsQueryGoesToATerminalThatDrawsImages(t *testing.T) {
	sess, att := sessionWithClient(t)
	defer sess.detach(att)

	sess.processChunk([]byte("painting"+graphicsQuery+"and carrying on"), 0)

	if got := outstandingRequests(sess); got != 1 {
		t.Fatalf("outstanding proxied requests = %d, want 1: a terminal that draws images is the one thing "+
			"that can answer this", got)
	}
	select {
	case q := <-att.queries:
		if string(q) != graphicsQuery {
			t.Errorf("the client was asked %q, want %q", q, graphicsQuery)
		}
	case <-time.After(2 * time.Second):
		t.Error("the query was recorded as outstanding but never sent to the client, so the program waits " +
			"for a reply nobody was asked for")
	}
}

// A query any terminal can answer is unaffected, which is the rest of the proxy's traffic: the background
// colour, the clipboard, terminfo capabilities. Gating those on image support would break every one of them
// on a terminal that draws no pictures.
func TestANonGraphicsQueryStillGoesToAQuietTerminal(t *testing.T) {
	sess, att := sessionWithQuietClient(t)
	defer sess.detach(att)

	const backgroundColour = "\x1b]11;?\x07"
	sess.processChunk([]byte("painting"+backgroundColour+"and carrying on"), 0)

	if got := outstandingRequests(sess); got != 1 {
		t.Errorf("outstanding proxied requests = %d, want 1: any terminal can report its background colour",
			got)
	}
}

// With both attached, the question goes to the one that can answer it.
//
// This is the reported configuration exactly: a kitty and a mobile ssh client on one session. The proxy picks
// the most recently attached client, and the phone attached second, so the naive rule asks the phone. Eligible
// clients are ranked among themselves instead.
func TestAGraphicsQueryPrefersTheTerminalThatDrawsImages(t *testing.T) {
	sess, drawing := sessionWithClient(t)
	defer sess.detach(drawing)

	// The second client, attached later and unable to draw: the one the old rule would have picked.
	quiet, err := sess.attach(nil, sess.reserveClient())
	if err != nil {
		t.Fatalf("second attach() error = %v", err)
	}
	defer sess.detach(quiet)

	sess.processChunk([]byte("painting"+graphicsQuery+"and carrying on"), 0)

	select {
	case q := <-quiet.queries:
		t.Fatalf("the terminal that cannot draw images was asked %q, which it prints as text", q)
	default:
	}
	select {
	case q := <-drawing.queries:
		if string(q) != graphicsQuery {
			t.Errorf("the drawing terminal was asked %q, want %q", q, graphicsQuery)
		}
	case <-time.After(2 * time.Second):
		t.Error("nobody was asked, so icat waits out its detection timeout and draws nothing even though a " +
			"terminal that can draw is attached")
	}
}
