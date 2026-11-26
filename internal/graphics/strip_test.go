package graphics

import (
	"slices"
	"strings"
	"testing"
)

// image is a complete transmission, the shape a program writes.
const image = "\x1b_Ga=T,f=24,s=2,v=2,i=7;AQIDAQIDAQIDAQID\x1b\\"

// The command goes, the text around it stays.
//
// Both halves matter. Removing the image is the point, and keeping everything else is what makes it safe to do
// to a live screen: a graphics command paints no text and moves no cursor, so what is left describes the same
// screen without the picture.
func TestStripperRemovesACommandAndKeepsTheRest(t *testing.T) {
	var s Stripper
	got := string(s.Strip([]byte("before" + image + "after")))
	if want := "beforeafter"; got != want {
		t.Errorf("Strip() = %q, want %q", got, want)
	}
	if s.Pending() {
		t.Error("something is held after a complete command")
	}
}

// Plain output passes through untouched, and as the same bytes: this runs on every chunk of every attachment,
// so the common case must not copy.
func TestStripperPassesOrdinaryOutputThrough(t *testing.T) {
	var s Stripper
	in := []byte("\x1b[2J\x1b[Hjust text\r\n")
	got := s.Strip(in)
	if string(got) != string(in) {
		t.Errorf("Strip() = %q, want %q", got, in)
	}
	if &got[0] != &in[0] {
		t.Error("ordinary output was copied; the fast path must return the input itself")
	}
}

// A command split across chunks is still removed whole, which a pty guarantees rather than risks: reads cap at
// 1022 bytes on darwin, so an image always arrives in pieces. A stripper that forgot between chunks would send
// the tail of every image, and a bare payload plus ST is text on any terminal.
func TestStripperRemovesACommandSplitAcrossChunks(t *testing.T) {
	for cut := 1; cut < len(image); cut++ {
		var s Stripper
		var out strings.Builder
		out.Write(s.Strip([]byte("before" + image[:cut])))
		if !s.Pending() {
			t.Errorf("split at %d: nothing held, so the rest of the command will be forwarded as text", cut)
		}
		out.Write(s.Strip([]byte(image[cut:] + "after")))
		if got, want := out.String(), "beforeafter"; got != want {
			t.Errorf("split at %d: stripped stream = %q, want %q", cut, got, want)
		}
		if s.Pending() {
			t.Errorf("split at %d: still holding after the command completed", cut)
		}
	}
}

// After a gap, what is held can never be completed: the bytes that would have finished it are the ones that
// were lost. Keeping them would strip the front of whatever arrives next.
func TestStripperResetDropsWhatIsHeld(t *testing.T) {
	var s Stripper
	s.Strip([]byte(image[:20]))
	if !s.Pending() {
		t.Fatal("nothing held, so this test proves nothing")
	}
	s.Reset()
	if s.Pending() {
		t.Error("still holding after Reset")
	}
	if got, want := string(s.Strip([]byte("plain"))), "plain"; got != want {
		t.Errorf("Strip() after Reset = %q, want %q", got, want)
	}
}

// Several commands in one chunk, which is what a chunked transmission looks like: icat splits an image into
// many commands and they arrive together.
func TestStripperRemovesEveryCommandInAChunk(t *testing.T) {
	var s Stripper
	chunk := "a" + image + "b" + image + "c"
	if got, want := string(s.Strip([]byte(chunk))), "abc"; got != want {
		t.Errorf("Strip() = %q, want %q", got, want)
	}
}

// A chunked transmission is loading from its first m=1 chunk until the chunk that ends it, whatever the chunk
// boundaries of the output, and a command boundary inside it is not an image boundary.
//
// That difference is what a client switching from stripped to forwarded output needs: switched between two
// chunks of one image, it got the rest without the first chunk, and its terminal drew nothing.
func TestStripperIsLoadingUntilTheLastChunkOfAnImage(t *testing.T) {
	first := "\x1b_Ga=T,q=2,f=24,s=2,v=2,i=7,m=1;AQIDAQID\x1b\\"
	middle := "\x1b_Ga=T,q=2,m=1;AQID\x1b\\"
	last := "\x1b_Ga=T,q=2,m=0;AQID\x1b\\"

	var s Stripper
	type state struct {
		Pending, Loading bool
	}
	var got []state
	for _, chunk := range []string{"text", first, middle[:5], middle[5:], last, "text"} {
		s.Strip([]byte(chunk))
		got = append(got, state{s.Pending(), s.Loading()})
	}
	want := []state{{false, false}, {false, true}, {true, true}, {false, true}, {false, false}, {false, false}}
	if !slices.Equal(got, want) {
		t.Errorf("after each chunk (Pending, Loading) = %v, want %v", got, want)
	}

	// Reset forgets it: after a gap the chunks that would end it may be the ones lost.
	s.Strip([]byte(first))
	s.Reset()
	if s.Loading() {
		t.Error("still loading after Reset")
	}
}
