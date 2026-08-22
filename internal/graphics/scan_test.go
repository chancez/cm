package graphics

import (
	"bytes"
	"strings"
	"testing"
)

// Ordinary output passes through untouched and nothing is held, which is the case every byte of a
// session takes.
func TestScanPassesThroughOrdinaryOutput(t *testing.T) {
	var s Scanner
	in := []byte("hello\x1b[1;2Hworld\x1b]133;A\x07")
	cmds, rest := scanParts(&s, in)

	if len(cmds) != 0 {
		t.Errorf("Scan() found %d commands in output with none", len(cmds))
	}
	if string(rest) != string(in) {
		t.Errorf("Scan() rest = %q, want the input unchanged", rest)
	}
	if s.Pending() != 0 {
		t.Errorf("Pending() = %d, want 0", s.Pending())
	}
}

// A command is removed from the stream, because forwarding a transmission that names a file lets a
// second reader race for a single-use file. That race is the reported EBADF.
func TestScanRemovesCommandsFromTheStream(t *testing.T) {
	var s Scanner
	in := []byte("before" + probeTempFile + "after")
	cmds, rest := scanParts(&s, in)

	if len(cmds) != 1 {
		t.Fatalf("Scan() found %d commands, want 1", len(cmds))
	}
	if cmds[0].Medium != MediumTempFile {
		t.Errorf("Medium = %q, want %q", cmds[0].Medium, MediumTempFile)
	}
	if string(rest) != "beforeafter" {
		t.Errorf("rest = %q, want %q: the command must not reach the client", rest, "beforeafter")
	}
}

// Several commands in one chunk all come out, since icat sends its three probes together.
func TestScanFindsSeveralCommands(t *testing.T) {
	var s Scanner
	in := []byte(probeDirect + probeTempFile + probeSharedMem)
	cmds, rest := scanParts(&s, in)

	if len(cmds) != 3 {
		t.Fatalf("Scan() found %d commands, want 3", len(cmds))
	}
	for i, want := range []uint32{1, 2, 3} {
		if cmds[i].ImageID != want {
			t.Errorf("command %d has ImageID %d, want %d", i, cmds[i].ImageID, want)
		}
	}
	if len(rest) != 0 {
		t.Errorf("rest = %q, want empty", rest)
	}
}

// A command split at every possible point has to survive, because a pty read caps at 1022 bytes on
// darwin so this is the guaranteed case rather than a rare one.
func TestScanReassemblesAcrossEverySplit(t *testing.T) {
	full := "before" + probeDirect + "after"
	for cut := 1; cut < len(full); cut++ {
		var s Scanner
		var got []Command
		var rest bytes.Buffer

		for _, part := range []string{full[:cut], full[cut:]} {
			cmds, r := scanParts(&s, []byte(part))
			got = append(got, cmds...)
			rest.Write(r)
		}

		if len(got) != 1 {
			t.Fatalf("split at %d: found %d commands, want 1", cut, len(got))
		}
		if got[0].ImageID != 1 {
			t.Errorf("split at %d: ImageID = %d, want 1", cut, got[0].ImageID)
		}
		if rest.String() != "beforeafter" {
			t.Errorf("split at %d: rest = %q, want %q", cut, rest.String(), "beforeafter")
		}
		if s.Pending() != 0 {
			t.Errorf("split at %d: Pending() = %d after completion, want 0", cut, s.Pending())
		}
	}
}

// A transmission arriving in many chunks, which is how a real image is sent.
func TestScanReassemblesAcrossManyChunks(t *testing.T) {
	payload := strings.Repeat("iVBORw0KGgo", 300)
	full := "\x1b_Ga=T,f=100,s=1712,v=1294,i=1;" + payload + "\x1b\\"

	var s Scanner
	var got []Command
	// 1022 bytes is the kernel's ceiling on a single pty read, so this is the real chunk size.
	for off := 0; off < len(full); off += 1022 {
		cmds, _ := scanParts(&s, []byte(full[off:min(off+1022, len(full))]))
		got = append(got, cmds...)
	}

	if len(got) != 1 {
		t.Fatalf("found %d commands across %d chunks, want 1", len(got), (len(full)+1021)/1022)
	}
	if string(got[0].Payload) != payload {
		t.Errorf("payload length = %d, want %d", len(got[0].Payload), len(payload))
	}
}

// A trailing introducer fragment must be held rather than forwarded, or the bytes completing it arrive
// with nothing to join.
func TestScanHoldsTrailingFragments(t *testing.T) {
	for _, frag := range []string{"\x1b", "\x1b_", "\x1b_G", "\x1b_Ga=T,i=1;AAA"} {
		var s Scanner
		cmds, rest := scanParts(&s, []byte("text" + frag))

		if len(cmds) != 0 {
			t.Errorf("fragment %q: found %d commands, want 0", frag, len(cmds))
		}
		if string(rest) != "text" {
			t.Errorf("fragment %q: rest = %q, want %q", frag, rest, "text")
		}
		if s.Pending() != len(frag) {
			t.Errorf("fragment %q: Pending() = %d, want %d", frag, s.Pending(), len(frag))
		}
	}
}

// An APC that is not graphics has to pass through: cm only claims the graphics protocol, and consuming
// another APC would break whatever uses it, such as a program setting a window title that way.
func TestScanLeavesOtherAPCAlone(t *testing.T) {
	var s Scanner
	in := []byte("\x1b_Xsomething else\x1b\\")
	cmds, rest := scanParts(&s, in)

	if len(cmds) != 0 {
		t.Errorf("found %d commands in a non-graphics APC", len(cmds))
	}
	if string(rest) != string(in) {
		t.Errorf("rest = %q, want the APC forwarded unchanged", rest)
	}
}

// Reset drops a held fragment, for a discontinuity where the completing bytes were lost.
func TestScanResetDropsHeldBytes(t *testing.T) {
	var s Scanner
	scanParts(&s, []byte("\x1b_Ga=T,i=1;AAA"))
	if s.Pending() == 0 {
		t.Fatal("expected a held fragment to reset")
	}

	s.Reset()
	if s.Pending() != 0 {
		t.Errorf("Pending() = %d after Reset, want 0", s.Pending())
	}

	// And the scanner still works afterwards rather than being left mid-parse.
	cmds, rest := scanParts(&s, []byte("plain"))
	if len(cmds) != 0 || string(rest) != "plain" {
		t.Errorf("after Reset: cmds = %d, rest = %q", len(cmds), rest)
	}
}

// An unterminated command must not buffer the rest of the session. The bound holds whatever a program
// does, and what it costs is one unrecognized command rather than unbounded memory.
func TestScanBoundsAHeldFragment(t *testing.T) {
	var s Scanner
	// An introducer with no terminator, fed until well past the bound.
	scanParts(&s, []byte("\x1b_Ga=T,i=1;"))
	for range 4 {
		scanParts(&s, bytes.Repeat([]byte("A"), maxHeld/2))
		if s.Pending() > maxHeld {
			t.Fatalf("Pending() = %d, over the %d bound", s.Pending(), maxHeld)
		}
	}
}

// The fast path must not allocate, since it runs on every chunk of session output.
func TestScanFastPathDoesNotCopy(t *testing.T) {
	var s Scanner
	in := []byte("ordinary output with no graphics at all")
	_, rest := scanParts(&s, in)

	// Same backing array means the chunk was passed through rather than rebuilt.
	if len(rest) > 0 && &rest[0] != &in[0] {
		t.Error("Scan() copied a chunk containing no graphics; the fast path should return the input")
	}
}

// scanParts adapts Scan's segments to the older two-value shape, so the tests below read as they did.
//
// A helper rather than a second method on Scanner: segments are the API precisely because separating the
// commands from the surrounding bytes loses the order they appeared in, and offering that shape on the
// type would invite the corruption it exists to prevent.
func scanParts(s *Scanner, p []byte) (cmds []Command, rest []byte) {
	segs := s.Scan(p)
	if segs == nil {
		return nil, p
	}
	for _, seg := range segs {
		if seg.Graphics {
			cmds = append(cmds, seg.Cmd)
			continue
		}
		rest = append(rest, seg.Data...)
	}
	return cmds, rest
}

// Segments come back in the order they appeared, which is the whole reason Scan returns segments rather
// than the commands and the leftover bytes separately.
//
// The separated form loses position: "BEFORE cmd1 MIDDLE cmd2 AFTER" collapses to "BEFOREMIDDLEAFTER"
// plus two commands, and any reassembly puts both commands at one end. That shipped briefly and was
// observed in a sandbox as a refused command's payload printed on the prompt line, with the probe beside
// it arriving payload-free: kitty answered "ENODATA: Insufficient image data: 0 < 3".
func TestScanSegmentsPreserveOrder(t *testing.T) {
	var s Scanner
	segs := s.Scan([]byte("BEFORE\x1b_Ga=T,i=1;AAAA\x1b\\MIDDLE\x1b_Ga=T,i=2;BBBB\x1b\\AFTER"))

	var got []string
	for _, seg := range segs {
		if seg.Graphics {
			got = append(got, "cmd"+string(rune('0'+seg.Cmd.ImageID)))
			continue
		}
		got = append(got, string(seg.Data))
	}

	want := []string{"BEFORE", "cmd1", "MIDDLE", "cmd2", "AFTER"}
	if len(got) != len(want) {
		t.Fatalf("segments = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("segment %d = %q, want %q (all: %v)", i, got[i], want[i], got)
		}
	}
}

// A segment's data must survive until the next Scan, since the caller writes it out after inspecting the
// whole result. The held buffer is compacted at the end of every call, so a segment aliasing it would be
// overwritten: that turned "text" into "\x1b_Gt" before the output buffer was separated.
func TestScanSegmentDataSurvivesCompaction(t *testing.T) {
	var s Scanner
	segs := s.Scan([]byte("text\x1b_G"))

	if len(segs) != 1 || segs[0].Graphics {
		t.Fatalf("segments = %+v, want one output segment", segs)
	}
	if string(segs[0].Data) != "text" {
		t.Errorf("segment data = %q, want %q: it aliases the buffer the held fragment was compacted into",
			segs[0].Data, "text")
	}
	if s.Pending() != 3 {
		t.Errorf("Pending() = %d, want 3 for the held introducer", s.Pending())
	}
}
