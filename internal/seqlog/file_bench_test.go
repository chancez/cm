package seqlog

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/chancez/cm/internal/seq"
)

// BenchmarkAppendAtCap measures one pty read appended to a log already at DefaultFileLimits, in each
// layout.
//
// A full-screen program like nvim positions the cursor rather than writing newlines, so its log reaches
// the byte ceiling long before the line limit. File then rewrites everything retained on every append:
// 4.1ms per 1022-byte read on an M3 Max, which is what made scrolling nvim under cm visibly slow.
func BenchmarkAppendAtCap(b *testing.B) {
	open := map[string]func(string) (Persisted[seq.Shim], error){
		"file": func(p string) (Persisted[seq.Shim], error) {
			return OpenFile[seq.Shim](p, DefaultFileLimits)
		},
		"dir": func(p string) (Persisted[seq.Shim], error) {
			return OpenDir[seq.Shim](p, DefaultFileLimits)
		},
	}
	for name, openLog := range open {
		b.Run(name, func(b *testing.B) {
			l, err := openLog(filepath.Join(b.TempDir(), "log"))
			if err != nil {
				b.Fatal(err)
			}
			defer l.Close()
			fill := bytes.Repeat([]byte("\x1b[1;1Hx"), 1<<16)
			for {
				oldest, next := l.Bounds()
				if int64(next-oldest) >= DefaultFileLimits.MaxBytes {
					break
				}
				if err := l.Append(fill); err != nil {
					b.Fatal(err)
				}
			}
			chunk := bytes.Repeat([]byte("x"), 1022)
			b.SetBytes(int64(len(chunk)))
			for b.Loop() {
				if err := l.Append(chunk); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
