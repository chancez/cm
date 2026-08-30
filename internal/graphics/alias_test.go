package graphics

import (
	"bytes"
	"testing"
)

// A segment must still describe its own command when Scan returns.
//
// Scan accumulates into one buffer and, at the end of a call, moves whatever it could not consume to the
// front of that same buffer. A graphics command's payload is not copied out, so it points into those bytes:
// the move overwrites the command the call is about to return. The contract is that a caller uses the
// result before calling Scan *again*, and this is Scan invalidating it before the caller gets it at all.
//
// It needs a chunk that both completes one command and holds part of the next, which is the ordinary case
// for an image: chunks arrive smaller than commands, so almost every arrival looks like this. A single
// small image completes in one chunk with nothing held and never sees it, which is why small images
// displayed correctly while large ones did not.
//
// Asserted after Scan returns, deliberately. Reading the payload inside the loop reads it before the move
// and passes while the caller sees rubble.
func TestASegmentSurvivesUntilScanReturns(t *testing.T) {
	first := []byte("\x1b_Ga=T,q=2,i=1,m=1;AAAABBBBCCCCDDDD\x1b\\")
	second := []byte("\x1b_Ga=T,q=2,m=0;EEEEFFFFGGGGHHHH\x1b\\")

	// One chunk carrying all of the first command and the head of the second, so the scanner completes one
	// and holds the rest.
	chunk := append(append([]byte(nil), first...), second[:12]...)

	var s Scanner
	segs := s.Scan(chunk)

	var got []Command
	for _, seg := range segs {
		if seg.Graphics {
			got = append(got, seg.Cmd)
		}
	}
	if len(got) != 1 {
		t.Fatalf("scanned %d commands, want 1", len(got))
	}
	if want := []byte("AAAABBBBCCCCDDDD"); !bytes.Equal(got[0].Payload, want) {
		t.Errorf("payload = %q, want %q: the buffer moved under the segment before Scan returned",
			got[0].Payload, want)
	}
	if want := "a=T,q=2,i=1,m=1"; got[0].Control != want {
		t.Errorf("control = %q, want %q", got[0].Control, want)
	}
	if !bytes.Equal(got[0].Raw, first) {
		t.Errorf("raw = %q, want %q", got[0].Raw, first)
	}

	// And the held remainder still completes the second command, so the fix cannot be to stop holding.
	segs = s.Scan(second[12:])
	got = got[:0]
	for _, seg := range segs {
		if seg.Graphics {
			got = append(got, seg.Cmd)
		}
	}
	if len(got) != 1 {
		t.Fatalf("the second command did not complete: %d commands", len(got))
	}
	if want := []byte("EEEEFFFFGGGGHHHH"); !bytes.Equal(got[0].Payload, want) {
		t.Errorf("second payload = %q, want %q", got[0].Payload, want)
	}
}

// Holding part of a command must not read as "no graphics here".
//
// A nil result tells the caller to forward the chunk unchanged, so returning nil while holding the start of
// a command makes those bytes go out twice: once raw, and again inside the command once it completes. The
// first image of a session is the one that hits it, because s.segs is nil only until something is appended
// to it, so it was the very first partial command in a session that got duplicated and every image after
// looked fine.
//
// Asserted as nil-versus-empty rather than by length, since that is the distinction the caller acts on.
func TestScanHoldingAPartialCommandReturnsEmptyNotNil(t *testing.T) {
	var s Scanner

	// The head of a command, terminator nowhere in sight.
	got := s.Scan([]byte("\x1b_Ga=T,q=2,i=1,m=1;AAAA"))
	if got == nil {
		t.Fatal("Scan() = nil while holding a partial command, so the caller forwards bytes the scanner " +
			"will also emit once the command completes")
	}
	if len(got) != 0 {
		t.Errorf("Scan() returned %d segments, want none: nothing is complete yet", len(got))
	}
	if s.Pending() == 0 {
		t.Error("nothing is held, so this test is not exercising the case")
	}

	// A chunk with no graphics at all still reports nil, which is what keeps the common path free.
	var plain Scanner
	if got := plain.Scan([]byte("just text")); got != nil {
		t.Errorf("Scan() = %v for ordinary output, want nil so the caller forwards it unchanged", got)
	}

	// And the held command completes without its head being emitted twice.
	rest := s.Scan([]byte("BBBB\x1b\\"))
	var cmds int
	var payload []byte
	for _, seg := range rest {
		if seg.Graphics {
			cmds++
			payload = seg.Cmd.Payload
		}
	}
	if cmds != 1 {
		t.Fatalf("completing the command produced %d commands, want 1", cmds)
	}
	if want := "AAAABBBB"; string(payload) != want {
		t.Errorf("payload = %q, want %q", payload, want)
	}
}
