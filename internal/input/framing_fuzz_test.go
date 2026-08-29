package input

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

// run is a maximal stretch of input with one destination, which is the form the routing decision
// actually takes: whether a given byte reaches the program as input or is matched against a query cm
// asked.
//
// Part boundaries are deliberately not compared. appendInputPart coalesces adjacent non-reply bytes, so
// the same routing can arrive as one part or several depending on where a chunk ended, and that
// difference is not a defect. Merging into runs compares what matters and ignores what does not.
type run struct {
	reply bool
	data  []byte
}

func runsOf(parts []Part) []run {
	var out []run
	for _, p := range parts {
		if n := len(out); n > 0 && out[n-1].reply == p.Reply {
			out[n-1].data = append(out[n-1].data, p.Data...)
			continue
		}
		out = append(out, run{reply: p.Reply, data: append([]byte(nil), p.Data...)})
	}
	return out
}

func showRuns(rs []run) string {
	var b strings.Builder
	for _, r := range rs {
		kind := "input"
		if r.reply {
			kind = "reply"
		}
		fmt.Fprintf(&b, "%s(%q) ", kind, r.data)
	}
	return b.String()
}

// frameAll feeds a stream through one framer in fixed-size chunks and returns every part it produced,
// including whatever the grace period finally releases.
//
// The flush at the end is part of the contract rather than tidying up: a stream ending inside a string
// control is held on purpose, and the bytes are released unchanged once no more are coming. Without it
// the conservation check would report the held tail as lost.
func frameAll(data []byte, chunk int) []Part { return frameAllExpecting(data, chunk, true) }

// frameAllExpecting is frameAll with the caller choosing whether cm is waiting for an answer.
//
// Almost every case here is about a reply, so frameAll passes true. The false variant is what a session with
// no outstanding question sees, and it is the one that has to keep Escape responsive.
func frameAllExpecting(data []byte, chunk int, expectReply bool) []Part {
	var f ReplyFramer
	var parts []Part
	now := time.Unix(0, 0)

	for off := 0; off < len(data); off += chunk {
		end := min(off+chunk, len(data))
		// Each read a little later than the last, which is what the grace period measures against.
		now = now.Add(time.Millisecond)
		parts = append(parts, f.Split(data[off:end], now, expectReply)...)
	}
	parts = append(parts, f.FlushExpired(now.Add(2*ReplyGrace))...)
	return parts
}

// FuzzFramingConservesEveryByte asserts the framer neither loses nor duplicates input.
//
// The first law of the input side: a keystroke the user typed must reach the program exactly once, and
// bytes cm did not ask for must not appear. Both directions have been violated here, by fragmented
// replies reaching the pty as text and by terminal events arriving after the program that wanted them
// exited.
func FuzzFramingConservesEveryByte(f *testing.F) {
	f.Add([]byte("hello"), 1)
	f.Add([]byte("\x1b]52;c;YWJj\x07"), 4)
	f.Add([]byte("\x1b[A\x1b[B\x1b[C"), 2)
	f.Add([]byte("\x1bP+q4D73\x1b\\rest"), 3)
	f.Add([]byte("\x1b"), 1)

	f.Fuzz(func(t *testing.T, data []byte, chunk int) {
		if len(data) == 0 {
			return
		}
		if chunk < 1 {
			chunk = 1
		}

		var got bytes.Buffer
		for _, p := range frameAll(data, chunk) {
			got.Write(p.Data)
		}
		if !bytes.Equal(got.Bytes(), data) {
			t.Fatalf("framing in %d-byte chunks changed the bytes.\n got %q\nwant %q",
				chunk, got.Bytes(), data)
		}
	})
}

// FuzzFramingRoutesIndependentlyOfChunking asserts that where a byte goes does not depend on where the
// read boundary fell.
//
// This is the property the reported OSC 52 defect violates. A pty read is capped at 1022 bytes on
// macOS, so a large clipboard reply always arrives split; classifying each read on its own sends the
// unterminated head to the program and mistakes the continuation for typing. Fed whole the same bytes
// are one reply. The routing has to be the same either way, and a table test cannot say that because
// the interesting split offsets depend on the payload.
func FuzzFramingRoutesIndependentlyOfChunking(f *testing.F) {
	f.Add([]byte("\x1b]52;c;"+strings.Repeat("YWJj", 20)+"\x07"), 7)
	f.Add([]byte("\x1b]11;rgb:2828/2c2c/3434\x1b\\"), 5)
	f.Add([]byte("typed\x1b[Amore"), 3)
	f.Add([]byte("\x1b_Gi=1;OK\x1b\\"), 2)

	// No exclusion any more. This target used to skip every input whose chunking ended on an ESC that begins
	// a string control, because that boundary defeated the framer and the fuzzer would otherwise report the
	// same gap forever. Holding an incomplete tail when a reply is expected closed it, so the fuzzer is free
	// to look at those boundaries too. Fed with expectReply true, which is the state the exclusion was about.
	f.Fuzz(func(t *testing.T, data []byte, chunk int) {
		if len(data) == 0 {
			return
		}
		if chunk < 1 {
			chunk = 1
		}

		whole := runsOf(frameAll(data, len(data)))
		split := runsOf(frameAll(data, chunk))

		if len(whole) != len(split) {
			t.Fatalf("routing depends on chunking: %d runs fed whole, %d fed in %d-byte chunks\n"+
				" whole: %s\n split: %s\n  input: %q",
				len(whole), len(split), chunk, showRuns(whole), showRuns(split), data)
		}
		for i := range whole {
			if whole[i].reply != split[i].reply || !bytes.Equal(whole[i].data, split[i].data) {
				t.Fatalf("routing depends on chunking at run %d, with %d-byte chunks\n"+
					" whole: %s\n split: %s\n  input: %q",
					i, chunk, showRuns(whole), showRuns(split), data)
			}
		}
	})
}

// TestLargeClipboardReplyArrivesAsOneReply is the measured case from the review, as a test.
//
// An OSC 52 clipboard response was measured at 18008 bytes against a macOS pty read cap of 1022, so it
// always arrives in at least 18 pieces. If the framer classifies each piece on its own, the program
// receives the response as though the user had typed it. Asserted as a single reply run rather than by
// counting parts, since part boundaries are not the contract.
func TestLargeClipboardReplyArrivesAsOneReply(t *testing.T) {
	// 18008 bytes total, matching the measurement, with a payload of base64-ish filler.
	const prefix = "\x1b]52;c;"
	const suffix = "\x07"
	payload := strings.Repeat("QUJDRA", (18008-len(prefix)-len(suffix))/6)
	reply := []byte(prefix + payload + suffix)

	// 1022 is the read cap that makes this always happen, not an arbitrary size.
	runs := runsOf(frameAll(reply, 1022))

	if len(runs) != 1 {
		t.Fatalf("an %d-byte OSC 52 reply arriving in 1022-byte reads produced %d runs, want 1:\n%s",
			len(reply), len(runs), showRuns(runs))
	}
	if !runs[0].reply {
		t.Errorf("the reply was routed to the program as input, so a clipboard response is typed into it")
	}
	if !bytes.Equal(runs[0].data, reply) {
		t.Errorf("the reply's bytes changed in transit: got %d bytes, want %d", len(runs[0].data), len(reply))
	}
}

// TestSplitIntroducerIsFramedWhenAReplyIsExpected covers the boundary that used to defeat the framer.
//
// A read ending between the ESC and the introducer after it left the lone ESC released as a keypress, so when
// the introducer arrived in the next read there was nothing to attach it to and the whole reply reached the
// program as text. Measured for every reply form:
//
//	ESC | ]52;c;YWJj BEL   -> input("\x1b"), input("]52;c;YWJj\a")
//	ESC ] | 52;c;YWJj BEL  -> reply("\x1b]52;c;YWJj\a")
//
// It is fixed by holding an incomplete tail *only when cm has a question outstanding with this client*.
// Holding one unconditionally was the reason this stayed open: pressing Escape alone produces a one-byte
// read, so it would have delayed every Escape by ReplyGrace. A lone ESC cannot begin a reply to a question
// nobody asked, so with nothing outstanding it still goes straight through.
func TestSplitIntroducerIsFramedWhenAReplyIsExpected(t *testing.T) {
	for _, tc := range []struct{ name, stream string }{
		{"OSC 52 clipboard reply", "\x1b]52;c;YWJj\x07"},
		{"DCS capability reply", "\x1bP+q4D73\x1b\\"},
		{"APC graphics reply", "\x1b_Gi=1;OK\x1b\\"},
		// A CSI reply, which beginsStringControl never covered: this is the answer to CSI 14t or 16t.
		{"CSI pixel size reply", "\x1b[4;600;800t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Split after the ESC, which is the boundary in question.
			afterESC := runsOf(splitOnceAt(tc.stream, 1))
			if len(afterESC) != 1 || !afterESC[0].reply {
				t.Errorf("split after the ESC gave %s, want a single reply: the lone ESC was released as a "+
					"keypress, so the rest arrives with nothing to attach it to and reaches the program as "+
					"text", showRuns(afterESC))
			}

			// And splitting one byte later still works, which is the control: it did before the fix, so a
			// change that broke it would be a regression this test has to catch.
			afterIntroducer := runsOf(splitOnceAt(tc.stream, 2))
			if len(afterIntroducer) != 1 || !afterIntroducer[0].reply {
				t.Errorf("split after the introducer gave %s, want a single reply", showRuns(afterIntroducer))
			}

			// The bytes are all still there either way.
			for _, parts := range [][]Part{splitOnceAt(tc.stream, 1), splitOnceAt(tc.stream, 2)} {
				var all []byte
				for _, p := range parts {
					all = append(all, p.Data...)
				}
				if !bytes.Equal(all, []byte(tc.stream)) {
					t.Errorf("bytes changed: got %q, want %q", all, tc.stream)
				}
			}
		})
	}
}

// TestEscapeIsNotDelayedWhenNoReplyIsExpected is the other half, and the reason the fix is conditional.
//
// Pressing Escape alone produces a one-byte read. If the framer held every incomplete tail, every Escape
// would wait out ReplyGrace before reaching the program, which is why this boundary was left open rather
// than closed with an unconditional holdback. With nothing outstanding the ESC must still go straight
// through.
func TestEscapeIsNotDelayedWhenNoReplyIsExpected(t *testing.T) {
	var f ReplyFramer
	now := time.Unix(0, 0)

	parts := f.Split([]byte("\x1b"), now, false)
	runs := runsOf(parts)
	if len(runs) != 1 || runs[0].reply || !bytes.Equal(runs[0].data, []byte("\x1b")) {
		t.Fatalf("a lone Escape with no query outstanding gave %s, want it delivered at once as input",
			showRuns(runs))
	}
	// And nothing is being held, so no later keypress can be absorbed into it.
	if _, holding := f.Deadline(); holding {
		t.Error("the framer is holding a fragment after releasing a lone Escape, so the next keypress " +
			"could be treated as its continuation")
	}
}

// An arrow key is released at once even while a reply is expected, which is what keeps the holdback narrow.
//
// ESC O A is a complete two-byte escape sequence followed by a letter, so the tracker reports nothing
// pending and the key does not wait. Without this the fix would delay cursor keys inside every query window.
func TestArrowKeysAreNotHeldWhileAReplyIsExpected(t *testing.T) {
	var f ReplyFramer
	now := time.Unix(0, 0)

	runs := runsOf(f.Split([]byte("\x1bOA"), now, true))
	if len(runs) != 1 || runs[0].reply || !bytes.Equal(runs[0].data, []byte("\x1bOA")) {
		t.Fatalf("an arrow key with a query outstanding gave %s, want it delivered at once as input",
			showRuns(runs))
	}
}

// splitOnceAt frames a stream delivered as exactly two reads, split at the given offset.
func splitOnceAt(stream string, at int) []Part {
	var f ReplyFramer
	now := time.Unix(0, 0)
	var parts []Part
	parts = append(parts, f.Split([]byte(stream[:at]), now, true)...)
	parts = append(parts, f.Split([]byte(stream[at:]), now.Add(time.Millisecond), true)...)
	return append(parts, f.FlushExpired(now.Add(2*ReplyGrace))...)
}
