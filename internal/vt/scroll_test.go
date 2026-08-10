//go:build cgo

package vt

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// This file guards against libghostty being built in Debug mode, which is not a tuning
// preference but a bug with a user-visible symptom.
//
// Zig defaults to Debug, and ghostty derives `slow_runtime_safety` from the optimize mode. In Debug
// that enables integrity verification which walks an entire page, and `insertLines` invokes it once
// per row it shifts, each call standing up a fresh DebugAllocator. A reverse index with the cursor
// on the top row routes through insertLines, so it cost 14ms at 50x120 against 10us for the same
// sequence anywhere else, scaling with cell count: 1.8ms at 10x40 and 78ms at 100x200.
//
// `less` emits home plus reverse index once per line when paging up, and plain lines when paging
// down. So `u` cost about 350ms of emulator time per half page while `d` cost about 8ms, which
// presented as cm being slow to scroll up and fine to scroll down. Fixed by passing
// -Doptimize=ReleaseSafe in the libghostty build, in mise.toml and Dockerfile.test.

// scrollLine is a line wide enough to be representative without being pathological.
func scrollLine(i int) string {
	return fmt.Sprintf("line %05d this is some text to make the line reasonably wide aaaaaaaaaaaaaaaa\x1b[m", i)
}

// pageDownBytes is what a pager emits to scroll down: plain lines, each ending in CRLF.
func pageDownBytes(rows int) string {
	var b strings.Builder
	for i := range rows {
		b.WriteString(scrollLine(i))
		b.WriteString("\r\n")
	}
	return b.String()
}

// pageUpBytes is what `less` actually emits to scroll up, captured from a real pty: home the
// cursor, reverse index to open a row at the top, then the line.
func pageUpBytes(rows int) string {
	var b strings.Builder
	for i := range rows {
		b.WriteString("\x1b[H\x1bM")
		b.WriteString(scrollLine(i))
		b.WriteString("\r\n")
	}
	return b.String()
}

// writeDuration reports the fastest of several runs of one Write into a freshly prepared
// terminal.
//
// Fresh each trial, because the state a pager sits in is what makes the difference measurable, and
// reusing a terminal across trials lets one trial's scrolling change the next one's starting point.
// Fastest rather than mean, since this is looking for a floor that differs by orders of magnitude
// and the fastest run is the one least polluted by scheduling.
func writeDuration(t *testing.T, rows, cols uint16, prefill, payload string) time.Duration {
	t.Helper()

	var best time.Duration
	for trial := range 5 {
		term, err := NewSessionTerminal(rows, cols, 2000)
		if err != nil {
			t.Fatalf("NewSessionTerminal() error = %v", err)
		}
		if err := term.Write([]byte(prefill)); err != nil {
			term.Close()
			t.Fatalf("prefill Write() error = %v", err)
		}

		start := time.Now()
		err = term.Write([]byte(payload))
		elapsed := time.Since(start)
		term.Close()
		if err != nil {
			t.Fatalf("Write() error = %v", err)
		}

		if trial == 0 || elapsed < best {
			best = elapsed
		}
	}
	return best
}

// TestScrollUpIsNotPathologicallySlowerThanScrollDown fails when libghostty is built in Debug.
//
// A ratio rather than an absolute duration, deliberately. An absolute threshold has to be
// calibrated to the slowest machine that will ever run it, which on fast hardware makes it useless
// and on slow hardware makes it flaky. The two directions do comparable amounts of work on
// comparable amounts of text, so their ratio is a property of the build rather than of the machine:
// measured at about 4x correctly built, against about 45x in Debug, both at 50x120.
//
// The bound is 15x, which is far above the real ratio and far below the broken one, so this fails on
// the bug rather than on a busy CI runner.
func TestScrollUpIsNotPathologicallySlowerThanScrollDown(t *testing.T) {
	const (
		rows, cols = 50, 120
		// A screenful plus scrollback: the state a pager is in when someone pages through a diff.
		prefillRows = 2100
		// Half a screen, which is what `d` and `u` move.
		pageRows = rows / 2
		maxRatio = 15.0
	)

	prefill := pageDownBytes(prefillRows)
	down := writeDuration(t, rows, cols, prefill, pageDownBytes(pageRows))
	up := writeDuration(t, rows, cols, prefill, pageUpBytes(pageRows))

	// Guards against dividing by a duration the clock reported as zero, which a correctly built
	// library can genuinely produce for this much work.
	if down <= 0 {
		down = time.Microsecond
	}
	ratio := float64(up) / float64(down)
	t.Logf("half page up=%v down=%v ratio=%.1fx",
		up.Round(time.Microsecond), down.Round(time.Microsecond), ratio)

	if ratio > maxRatio {
		t.Errorf("scrolling up took %.1fx as long as scrolling down (up=%v down=%v), want at most %.0fx: "+
			"libghostty is probably built in Debug mode, where slow_runtime_safety verifies a whole page "+
			"per row shifted. Check -Doptimize=ReleaseSafe in mise.toml and Dockerfile.test.",
			ratio, up.Round(time.Microsecond), down.Round(time.Microsecond), maxRatio)
	}
}

// TestReverseIndexAtTopIsCheap is the same check at the level of the single operation responsible.
//
// Worth having in addition to the ratio above, because it names the exact sequence rather than a
// workload: a reverse index with the cursor on the top row scrolls the screen, and anywhere else it
// only moves the cursor. Debug made the first cost about 1400x the second, so comparing them
// isolates the regression from anything else a page of output does.
func TestReverseIndexAtTopIsCheap(t *testing.T) {
	const (
		rows, cols = 50, 120
		maxRatio   = 60.0
	)

	prefill := pageDownBytes(2100)
	// Cursor left at the bottom, so the reverse index only moves it up.
	cheap := writeDuration(t, rows, cols, prefill, "\x1bM")
	// Homed first, so the reverse index has to scroll.
	scrolling := writeDuration(t, rows, cols, prefill, "\x1b[H\x1bM")

	if cheap <= 0 {
		cheap = time.Microsecond
	}
	ratio := float64(scrolling) / float64(cheap)
	t.Logf("reverse index at top=%v elsewhere=%v ratio=%.1fx",
		scrolling.Round(time.Microsecond), cheap.Round(time.Microsecond), ratio)

	if ratio > maxRatio {
		t.Errorf("a reverse index at the top row took %.1fx one elsewhere (top=%v elsewhere=%v), "+
			"want at most %.0fx: libghostty is probably built in Debug mode. Check "+
			"-Doptimize=ReleaseSafe in mise.toml and Dockerfile.test.",
			ratio, scrolling.Round(time.Microsecond), cheap.Round(time.Microsecond), maxRatio)
	}
}
