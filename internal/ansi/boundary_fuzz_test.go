package ansi

import (
	"testing"
)

// FuzzTrackerChunking asserts the one property that matters about this type: the answer cannot depend
// on how the bytes were chunked.
//
// That is the shape of nearly every escape-sequence bug in cm. A pty read ends wherever the kernel
// buffer did, so every consumer sees arbitrary splits, and a state machine that is right on whole
// sequences and wrong on split ones passes every hand-written table test. Three cases were written by
// hand for this; the fuzzer covers the ones nobody thought of.
func FuzzTrackerChunking(f *testing.F) {
	f.Add([]byte("hello"), 1)
	f.Add([]byte("\x1b[38:2:1:2:3mtext"), 4)
	f.Add([]byte("\x1b]7;file://host/tmp\x1b\\"), 9)
	f.Add([]byte("\x1bP+q4D73\x1b\\"), 3)
	f.Add([]byte("\x1b_Gf=100;payload\x1b\\"), 7)
	f.Add([]byte("a\x1b[1mb\x1b]2;t\x07c"), 2)

	f.Fuzz(func(t *testing.T, data []byte, split int) {
		if len(data) == 0 {
			return
		}
		// A split size of at least one, bounded by the input, so the loop terminates.
		if split < 1 {
			split = 1
		}

		var whole Tracker
		whole.Feed(data)

		var chunked Tracker
		for off := 0; off < len(data); off += split {
			end := min(off+split, len(data))
			chunked.Feed(data[off:end])
		}

		if whole.InSequence() != chunked.InSequence() {
			t.Fatalf("InSequence() = %v fed whole, %v fed in chunks of %d, for %q",
				whole.InSequence(), chunked.InSequence(), split, data)
		}
	})
}

// FuzzStripperChunking asserts the same property for the filter: what it emits cannot depend on how the
// input arrived.
//
// Stripper is stateful precisely so a sequence split across writes is still recognized, and a
// regression there is invisible to a table test that feeds whole strings.
func FuzzStripperChunking(f *testing.F) {
	f.Add([]byte("\x1b[31mred\x1b[0m\n"), 3)
	f.Add([]byte("a\x1bP+q4D73\x1b\\b"), 2)
	f.Add([]byte("\x1b]133;C\x07running"), 5)
	f.Add([]byte("text with no sequences"), 4)

	f.Fuzz(func(t *testing.T, data []byte, split int) {
		if len(data) == 0 {
			return
		}
		if split < 1 {
			split = 1
		}

		whole := string(Strip(data))

		var chunked stringWriter
		s := NewStripper(&chunked)
		for off := 0; off < len(data); off += split {
			end := min(off+split, len(data))
			if _, err := s.Write(data[off:end]); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
		}

		if whole != chunked.String() {
			t.Fatalf("Strip() = %q whole, %q in chunks of %d, for %q",
				whole, chunked.String(), split, data)
		}
	})
}

// stringWriter collects what a Stripper emits.
type stringWriter struct{ b []byte }

func (w *stringWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func (w *stringWriter) String() string { return string(w.b) }
