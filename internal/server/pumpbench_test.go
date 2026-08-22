package server

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/osc"
)

// Benchmarks for the per-chunk work the output pump does on every read from a shim.
//
// These exist to make a change to that path measurable rather than argued about. Intercepting kitty
// graphics means scanning for APC ahead of the model and the client fan-out, which is the one place in cm
// where a cost is paid per byte of session output rather than per attach or per command. A regression here
// is a session that feels slow, and the last one of those was diagnosed only after someone noticed paging
// in `less` had become unusable: a missing -Doptimize made it 145-166ms per keypress against 32-91us.
//
// The chunk size is fixed at the kernel's ceiling rather than chosen. A single read from a pty master
// returns at most 1022 bytes on darwin whatever buffer is passed, measured at buffer sizes 4096 and 65536,
// so a realistic chunk is that size and the number of chunks is what scales with output.
//
// What is deliberately *not* benchmarked here is the terminal model itself. libghostty is called once per
// pty read and the cost is about 100ns per 4KB, so it is not the thing at risk; the trackers and any new
// scan are, because they inspect every byte in Go.

// ptyChunk is the largest a single read from a pty master returns on darwin.
const ptyChunk = 1022

// benchChunk builds a chunk of plain output, which is the overwhelmingly common case: no escape sequences
// at all, so every tracker takes its early-out path.
func benchChunk(n int) []byte {
	return []byte(strings.Repeat("x", n))
}

// benchChunkWithPrompt builds a chunk carrying the OSC 133 markers a shell emits around a command, which
// is what the trackers actually have to parse.
func benchChunkWithPrompt(n int) []byte {
	const marker = "\x1b]133;A\x07prompt$ \x1b]133;C\x07"
	body := strings.Repeat("y", max(0, n-len(marker)))
	return []byte(marker + body)
}

// benchChunkWithGraphics builds a chunk carrying a kitty graphics command, so the cost of a scan that has
// to recognize APC can be compared against one that does not.
//
// Sized like a real transmission chunk rather than a probe: icat sends image data as chunked base64 and a
// captured transmission was 131109 bytes in total, so a chunk of it is entirely payload with no escape
// sequence to find until the end.
func benchChunkWithGraphics(n int) []byte {
	const intro = "\x1b_Ga=T,q=2,f=100,m=1;"
	body := strings.Repeat("iVBORw0KGgo", max(1, (n-len(intro))/11))
	return []byte(intro + body)
}

// BenchmarkCommandTrackerFeed measures the OSC 133 scan on plain output, which is the path every byte of
// a session's output takes and the one an added scan would sit beside.
func BenchmarkCommandTrackerFeed(b *testing.B) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"plain", benchChunk(ptyChunk)},
		{"prompt", benchChunkWithPrompt(ptyChunk)},
		{"graphics", benchChunkWithGraphics(ptyChunk)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var t osc.CommandTracker
			b.SetBytes(int64(len(tc.data)))
			b.ResetTimer()
			for range b.N {
				t.Feed(tc.data)
			}
		})
	}
}

// BenchmarkReportTrackerFeed measures the scan for cm's own OSC 25453 reports, the second per-byte pass.
func BenchmarkReportTrackerFeed(b *testing.B) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"plain", benchChunk(ptyChunk)},
		{"graphics", benchChunkWithGraphics(ptyChunk)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var t osc.ReportTracker
			b.SetBytes(int64(len(tc.data)))
			b.ResetTimer()
			for range b.N {
				t.Feed(tc.data)
			}
		})
	}
}

// BenchmarkBoundaryTrackerFeed measures the third per-byte pass, which records where each command's
// output begins.
func BenchmarkBoundaryTrackerFeed(b *testing.B) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"plain", benchChunk(ptyChunk)},
		{"prompt", benchChunkWithPrompt(ptyChunk)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			t := osc.NewBoundaryTracker(0)
			b.SetBytes(int64(len(tc.data)))
			b.ResetTimer()
			for range b.N {
				t.Feed(tc.data)
			}
		})
	}
}

// BenchmarkRewritePromptRedraw measures the one transform applied to every chunk, which returns its input
// untouched when there is no prompt marker and so shows what an early-out costs.
func BenchmarkRewritePromptRedraw(b *testing.B) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"plain", benchChunk(ptyChunk)},
		{"prompt", benchChunkWithPrompt(ptyChunk)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.SetBytes(int64(len(tc.data)))
			b.ResetTimer()
			for range b.N {
				osc.RewritePromptRedraw(tc.data)
			}
		})
	}
}

// BenchmarkPumpPerChunkScans measures every per-byte pass the pump makes, together, which is the number a
// change to this path should be compared against.
//
// Deliberately not the whole pump: appending to the log and feeding the emulator are measured elsewhere or
// dominated by I/O, and including them would bury the scans this is about. What is here is the work that
// happens between receiving a chunk and handing it on.
func BenchmarkPumpPerChunkScans(b *testing.B) {
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"plain", benchChunk(ptyChunk)},
		{"prompt", benchChunkWithPrompt(ptyChunk)},
		{"graphics", benchChunkWithGraphics(ptyChunk)},
	} {
		b.Run(tc.name, func(b *testing.B) {
			var cmds osc.CommandTracker
			var reports osc.ReportTracker
			var gfx graphics.Scanner
			boundaries := osc.NewBoundaryTracker(0)
			b.SetBytes(int64(len(tc.data)))
			b.ResetTimer()
			for range b.N {
				cmds.Feed(tc.data)
				reports.Feed(tc.data)
				// The graphics scan, in the position the pump runs it: after the trackers and ahead of
				// the prompt rewrite, since a rewrite inside an image payload would corrupt it.
				data := tc.data
				if segs := gfx.Scan(tc.data); segs != nil {
					data = joinSegments(segs)
				}
				data = osc.RewritePromptRedraw(data)
				boundaries.Feed(data)
			}
		})
	}
}

// BenchmarkImageTransmission measures a whole image transmission arriving as pty-sized chunks, which is
// what interception would have to keep up with.
//
// The size comes from a captured `kitten icat` run: 131109 bytes for one image, which is 129 chunks at the
// pty ceiling. Reported per operation rather than per byte so the cost of displaying one image is legible.
func BenchmarkImageTransmission(b *testing.B) {
	const transmission = 131109
	chunks := make([][]byte, 0, transmission/ptyChunk+1)
	for remaining := transmission; remaining > 0; remaining -= ptyChunk {
		chunks = append(chunks, benchChunkWithGraphics(min(ptyChunk, remaining)))
	}
	b.Logf("one image = %d bytes in %d chunks", transmission, len(chunks))

	var cmds osc.CommandTracker
	var reports osc.ReportTracker
	var gfx graphics.Scanner
	boundaries := osc.NewBoundaryTracker(0)
	b.SetBytes(int64(transmission))
	b.ResetTimer()
	for range b.N {
		for _, c := range chunks {
			cmds.Feed(c)
			reports.Feed(c)
			data := c
			if segs := gfx.Scan(c); segs != nil {
				data = joinSegments(segs)
			}
			data = osc.RewritePromptRedraw(data)
			boundaries.Feed(data)
		}
	}
}

// joinSegments rebuilds a chunk from segments, standing in for what handleGraphics does.
//
// The benchmark measures the scan rather than the handling, so commands are re-emitted as they arrived
// instead of being resolved: reading a transfer file would measure the filesystem.
func joinSegments(segs []graphics.Segment) []byte {
	var out []byte
	for _, seg := range segs {
		if seg.Graphics {
			out = append(out, seg.Cmd.Raw...)
			continue
		}
		out = append(out, seg.Data...)
	}
	return out
}
