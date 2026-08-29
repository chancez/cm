package osc

import (
	"bytes"
	"strings"
	"testing"
)

// TestRewritePromptRedrawIsLostWhenAMarkerIsSplit sweeps the chunk boundary through a prompt marker and
// records exactly which offsets lose the rewrite.
//
// The rewrite runs per chunk of pty output, and a read boundary lands wherever the kernel put it, so every
// offset here is an ordinary delivery. The function is stateless and passes an unterminated marker through
// unchanged, which its comment defends on the grounds that "the remainder arrives in the next chunk". It
// does, and by then the introducer has already gone out unrewritten, so nothing matches it and the rewrite
// simply does not happen.
//
// The consequence is in the function's own comment rather than inferred: a terminal that believes
// `redraw=1` clears the prompt lines on resize and waits for the shell to repaint them, and through a
// multiplexer that repaint arrives in the pty's coordinates rather than the window's, so the prompt is
// cleared and does not come back.
//
// The window is stated precisely because that is the useful part. A split before the marker or after its
// terminator is harmless; a split anywhere inside it loses the rewrite. For the first fixture that is
// offsets 7 through 14 of a 22-byte stream, and offset 7 is the byte after the ESC.
//
// This function is deliberately left stateless, and the gap is closed a level up instead: the output pump
// holds a partial marker back so this is never handed one. See PartialMarkerLen and
// Session.promptPartial, and TestHoldingBackRestoresTheRewriteAtEveryBoundary for the same sweep with the
// holdback applied, which passes at every offset.
//
// The holdback belongs there rather than here because it has to happen before the graphics transform, so
// the held bytes are still the shim's and lastSeq can decline to count them. Holding after that transform
// would mean mapping post-transform lengths back to shim positions, which is the two-numbering-spaces
// mistake in a new place.
//
// Kept as a record of what this function does on its own, so that if someone later feeds it a split marker
// from a new call site the consequence is written down rather than rediscovered.
func TestRewritePromptRedrawIsLostWhenAMarkerIsSplit(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
	}{
		{name: "BEL terminated", stream: "before\x1b]133;A\x07prompt$ "},
		{name: "ST terminated", stream: "before\x1b]133;A\x1b\\prompt$ "},
		{name: "already redraw=1", stream: "before\x1b]133;A;redraw=1\x07prompt$ "},
		{name: "other parameters", stream: "before\x1b]133;A;cl=m;redraw=1;k=i\x07prompt$ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			whole := string(RewritePromptRedraw([]byte(tc.stream)))

			// The control. Without it a fixture that is not a marker at all would satisfy everything below,
			// since "the rewrite did not happen" is trivially true of a stream it never acts on.
			if !strings.Contains(whole, "redraw=0") {
				t.Fatalf("the whole stream rewrote to %q, which carries no redraw=0: this fixture is not a "+
					"marker the function acts on, so the sweep proves nothing", whole)
			}

			// The marker's extent: from its introducer to just past its terminator.
			start := strings.Index(tc.stream, "\x1b]133;A")
			if start < 0 {
				t.Fatalf("fixture has no prompt marker")
			}
			end, termLen := oscEnd([]byte(tc.stream[start:]))
			if end < 0 {
				t.Fatalf("fixture's marker is unterminated")
			}
			markerEnd := start + end + termLen

			var lost, kept []int
			for split := 1; split < len(tc.stream); split++ {
				var got bytes.Buffer
				got.Write(RewritePromptRedraw([]byte(tc.stream[:split])))
				got.Write(RewritePromptRedraw([]byte(tc.stream[split:])))

				if got.String() == whole {
					kept = append(kept, split)
					continue
				}
				lost = append(lost, split)
			}

			// Inside the marker the rewrite is lost. Every such offset, not merely one, since a fix that
			// closed only the ESC boundary would leave the rest.
			for split := start + 1; split < markerEnd; split++ {
				if !contains(lost, split) {
					t.Errorf("a split at %d inside the marker (bytes %d..%d) kept the rewrite, so the gap "+
						"this test pins is partly closed. Update it deliberately rather than leaving it "+
						"describing behaviour that has changed.", split, start, markerEnd)
				}
			}

			// Outside it, chunking changes nothing, which is what makes the window above a real boundary
			// rather than the function being broken everywhere.
			for _, split := range kept {
				if split > start && split < markerEnd {
					t.Errorf("split at %d is inside the marker but kept the rewrite", split)
				}
			}
			if len(kept) == 0 {
				t.Error("no split preserved the rewrite, so the function is chunk-sensitive everywhere " +
					"rather than only across a marker, and this test is describing the wrong thing")
			}

			t.Logf("marker at bytes %d..%d of %d; the rewrite is lost at splits %v",
				start, markerEnd, len(tc.stream), lost)
		})
	}
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
