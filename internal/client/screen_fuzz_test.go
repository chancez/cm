package client

import (
	"bytes"
	"log/slog"
	"testing"

	"github.com/chancez/cm/internal/ansi"
)

// fuzzTitle is the injection the fuzzer uses: a complete OSC, so any complaint is about where it landed
// rather than what it contains. Chosen to be improbable in generated output, and inputs containing it
// are rejected below, so splitting the emitted stream on it is unambiguous.
const fuzzTitle = "\x1b]2;FUZZTITLE\x07"

// FuzzScreenNeverSplits drives the real writer with generated output and generated injection points,
// then checks the invariant against what the terminal actually received.
//
// This is the generated form of the four cases in screen_test.go. Those cover the split that happened
// to occur in one captured failure; the property worth stating is that NO arrangement of chunk
// boundaries and injections can interleave them, which a table cannot say.
//
// The first version of this fuzzer was worthless and it is worth recording why, because the mistake is
// easy and it looks like a passing test. Its oracle removed the injected bytes from the stream and
// compared the remainder to the program's output. That passes whatever the placement, because removing
// the injection restores the original either way, so the fuzzer ran clean against a deliberately broken
// screen. Byte conservation and correct placement are different properties and this needs both.
func FuzzScreenNeverSplits(f *testing.F) {
	// Seeds: the reported case, and shapes a real TUI produces.
	f.Add([]byte(" 30 \x1b(B\x1b[m\x1b[38:2:232:102:113m-export"), 20, 3)
	f.Add([]byte("\x1b]7;file://host/tmp\x1b\\text"), 8, 2)
	f.Add([]byte("\x1bP+q4D73\x1b\\\x1b[1mbold"), 5, 4)
	f.Add([]byte("plain text with no sequences at all"), 7, 5)
	f.Add([]byte("\x1b[?1049h\x1b[H\x1b[2J\x1b[38:2:1:2:3mx"), 6, 9)

	f.Fuzz(func(t *testing.T, out []byte, chunk int, every int) {
		if len(out) == 0 {
			return
		}
		// The reconstruction below splits on the title, so output containing it is ambiguous.
		if bytes.Contains(out, []byte(fuzzTitle)) {
			return
		}
		// Bounded so the loop terminates.
		if chunk < 1 {
			chunk = 1
		}
		if every < 1 {
			every = 1
		}

		var got bytes.Buffer
		scr := newScreen(&got, true, slog.New(discardLogHandler{}))

		n := 0
		for off := 0; off < len(out); off += chunk {
			end := min(off+chunk, len(out))
			if err := scr.session(out[off:end]); err != nil {
				t.Fatalf("session() error = %v", err)
			}
			n++
			if n%every == 0 {
				if err := scr.inject([]byte(fuzzTitle)); err != nil {
					t.Fatalf("inject() error = %v", err)
				}
			}
		}
		// A final complete sequence releases anything still held, which is what a real stream's next
		// chunk does. Without it a transcript ending mid-sequence looks like an injection that never
		// arrived, and that is correct behaviour rather than a defect.
		const flush = "\x1b[0m"
		if err := scr.session([]byte(flush)); err != nil {
			t.Fatalf("session() error = %v", err)
		}

		stream := got.Bytes()

		// Placement: reconstruct the transcript by splitting the emitted stream on the known injection,
		// then apply the same validator the e2e transcript uses. This is the property the first version of
		// this fuzzer missed.
		if problems := ansi.Validate(splitOnInjection(stream, []byte(fuzzTitle))); len(problems) > 0 {
			t.Fatalf("%v\nterminal received %s", problems[0], quote(stream))
		}

		// Conservation: with the injections removed, what the terminal received is exactly what the
		// program wrote. Catches a dropped or duplicated byte, which placement alone cannot see and which
		// the two sequence-number spaces have caused twice.
		want := append(append([]byte(nil), out...), flush...)
		if cleaned := bytes.ReplaceAll(stream, []byte(fuzzTitle), nil); !bytes.Equal(cleaned, want) {
			t.Fatalf("with injections removed the terminal received %s, want %s",
				quote(cleaned), quote(want))
		}
	})
}

// splitOnInjection turns an emitted stream back into a transcript, given the exact bytes that were
// injected.
//
// Reconstructed rather than read from the recorder so this runs without the cm_testhooks tag, which
// keeps the fuzzer in the ordinary test run rather than in a separate one nobody remembers to do.
func splitOnInjection(stream, injected []byte) []ansi.Write {
	var writes []ansi.Write
	for {
		i := bytes.Index(stream, injected)
		if i < 0 {
			if len(stream) > 0 {
				writes = append(writes, ansi.Write{Data: stream})
			}
			return writes
		}
		if i > 0 {
			writes = append(writes, ansi.Write{Data: stream[:i]})
		}
		writes = append(writes, ansi.Write{Data: injected, Injected: true})
		stream = stream[i+len(injected):]
	}
}

// quote renders a byte stream so a failure is readable, since these are mostly escape sequences.
func quote(p []byte) string {
	out := make([]byte, 0, len(p)+2)
	out = append(out, '"')
	for _, b := range p {
		switch {
		case b == 0x1b:
			out = append(out, []byte("<ESC>")...)
		case b == 0x07:
			out = append(out, []byte("<BEL>")...)
		case b < 0x20 || b > 0x7e:
			out = append(out, []byte("<?>")...)
		default:
			out = append(out, b)
		}
	}
	return string(append(out, '"'))
}
