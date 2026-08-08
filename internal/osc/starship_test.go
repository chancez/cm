package osc

import (
	"bytes"
	"testing"
)

// A real prompt stream must survive byte for byte apart from the redraw parameter.
//
// Reproduces the shape starship emits: an OSC 133 prompt marker, SGR styling, and a relative
// cursor move to draw a right-aligned segment. The observed symptom was a cursor move losing its
// ESC and rendering as the literal text "[183D" beside the prompt.
func TestRewritePreservesStarshipPrompt(t *testing.T) {
	prompt := []byte(
		"\x1b]133;A\x07" +
			"\x1b[m\x1b[22;1;33mchancez\x1b[22;34m~/p/cm\x1b[39m \x1b[35m\xe2\x9d\xaf\x1b[39m " +
			"\x1b[183D\x1b[32mmain\x1b[0m",
	)

	got := RewritePromptRedraw(prompt)

	// The only permitted change is the redraw parameter being added to the OSC 133 marker.
	want := bytes.Replace(prompt, []byte("\x1b]133;A\x07"), []byte("\x1b]133;A;redraw=0\x07"), 1)
	if !bytes.Equal(got, want) {
		t.Errorf("prompt was altered beyond the redraw parameter:\n got  = %q\n want = %q", got, want)
	}

	// And specifically: the cursor move must keep its escape.
	if bytes.Contains(got, []byte("[183D")) && !bytes.Contains(got, []byte("\x1b[183D")) {
		t.Error("the cursor move lost its ESC, so it would render as literal text")
	}
	if n := bytes.Count(got, []byte{0x1b}); n != bytes.Count(prompt, []byte{0x1b}) {
		t.Errorf("ESC count changed from %d to %d", bytes.Count(prompt, []byte{0x1b}), n)
	}
}

// The same prompt arriving split across reads, at every possible boundary.
//
// This is the case a single-buffer test cannot see: output arrives in arbitrary chunks, and a
// rewriter that holds back or consumes a partial sequence loses bytes only at specific splits.
func TestRewriteStarshipPromptAcrossEverySplit(t *testing.T) {
	prompt := []byte(
		"\x1b]133;A\x07\x1b[22;1;33mchancez\x1b[39m \x1b[183D\x1b[32mmain\x1b[0m",
	)
	wantESC := bytes.Count(prompt, []byte{0x1b})

	for split := 0; split <= len(prompt); split++ {
		var out bytes.Buffer
		out.Write(RewritePromptRedraw(prompt[:split]))
		out.Write(RewritePromptRedraw(prompt[split:]))

		if n := bytes.Count(out.Bytes(), []byte{0x1b}); n != wantESC {
			t.Errorf("split at %d: ESC count %d, want %d\n got = %q", split, n, wantESC, out.Bytes())
		}
	}
}
