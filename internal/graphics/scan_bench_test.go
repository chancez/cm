package graphics

import (
	"strings"
	"testing"
)

// What the graphics scan adds to the output pump, which is the number that decides whether
// intercepting on the hot path is acceptable.
//
// Compared against the pump's existing per-chunk work, measured on the same machine before this
// existed: three OSC trackers and a prompt rewrite cost 3.580us for a 1022 byte chunk of plain output.
// A fourth pass should look like the cheapest of those rather than the most expensive, and the fast
// path here should be closer to RewritePromptRedraw's 17.91ns early-out than to a tracker's 1.2us.
//
// Chunk size is the kernel's ceiling on a single pty read on darwin, so it is the real unit rather than
// a chosen one.
const benchChunkSize = 1022

func benchPlain() []byte { return []byte(strings.Repeat("x", benchChunkSize)) }

// A chunk of image data, which is what most chunks of a transmission look like: payload with no
// introducer to find.
func benchPayloadChunk() []byte {
	return []byte(strings.Repeat("iVBORw0KGgo", benchChunkSize/11))
}

// A chunk that opens a transmission, so the scan parses a control section.
func benchCommandChunk() []byte {
	const intro = "\x1b_Ga=T,q=2,f=100,m=1,s=1712,v=1294;"
	return []byte(intro + strings.Repeat("iVBORw0KGgo", (benchChunkSize-len(intro))/11) + "\x1b\\")
}

func BenchmarkScan(b *testing.B) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		// The overwhelmingly common case: no graphics anywhere. This is the one that must stay cheap,
		// because it is what every session pays on every chunk forever.
		{"plain", benchPlain()},
		// Mid-transmission payload, no introducer.
		{"payload", benchPayloadChunk()},
		// A complete command, so the control section is parsed.
		{"command", benchCommandChunk()},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var s Scanner
			b.SetBytes(int64(len(tc.data)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				s.Scan(tc.data)
				s.Reset()
			}
		})
	}
}

// A whole image arriving as pty-sized chunks, so the cost of displaying one image is legible next to
// the pump's ImageTransmission baseline of 460.5us for the same 131109 bytes.
func BenchmarkScanImageTransmission(b *testing.B) {
	const total = 131109
	payload := strings.Repeat("iVBORw0KGgo", total/11)
	full := "\x1b_Ga=T,q=2,f=100,s=1712,v=1294,i=1;" + payload + "\x1b\\"

	chunks := make([][]byte, 0, len(full)/benchChunkSize+1)
	for off := 0; off < len(full); off += benchChunkSize {
		chunks = append(chunks, []byte(full[off:min(off+benchChunkSize, len(full))]))
	}
	b.Logf("one image = %d bytes in %d chunks", len(full), len(chunks))

	b.SetBytes(int64(len(full)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var s Scanner
		for _, c := range chunks {
			s.Scan(c)
		}
	}
}

// The store's cost per transmission, which is paid alongside the scan.
func BenchmarkStoreAdd(b *testing.B) {
	cmd, _, ok := Parse(benchCommandChunk())
	if !ok {
		b.Fatal("Parse() failed on the benchmark chunk")
	}

	b.SetBytes(int64(len(cmd.Payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		s := NewStore(0)
		s.Add(cmd)
	}
}
