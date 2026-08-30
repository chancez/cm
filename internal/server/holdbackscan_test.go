package server

import (
	"encoding/base64"
	"testing"

	"github.com/chancez/cm/internal/ansi"
	"github.com/chancez/cm/internal/graphics"
)

// processChunk holds back a trailing partial sequence before the graphics scanner sees anything, and the
// two holdbacks have to compose. This drives just that pairing, because the scanner alone is correct at
// every read size (TestScannerKeepsCommandBoundariesAcrossPtySizedReads) while the pump is not.
func TestTheAnsiHoldbackDoesNotCorruptGraphicsCommands(t *testing.T) {
	const w, h = 200, 120
	px := make([]byte, w*h*3)
	for i := range px {
		px[i] = byte(i % 251)
	}
	enc := base64.RawStdEncoding.EncodeToString(px)

	var stream []byte
	var want []int
	const step = 4096
	first := true
	for i := 0; i < len(enc); i += step {
		end := min(i+step, len(enc))
		more := "0"
		if end < len(enc) {
			more = "1"
		}
		control := "a=T,q=2,m=" + more
		if first {
			control = "a=T,q=2,f=24,s=200,v=120,i=7,m=" + more
			first = false
		}
		stream = append(stream, "\x1b_G"+control+";"+enc[i:end]+"\x1b\\"...)
		want = append(want, end-i)
	}

	for _, size := range []int{511, 1021, 1022, 1023, 2048} {
		t.Run("reads of "+itoaLocal(size), func(t *testing.T) {
			// The same two stages processChunk runs, in the same order.
			var partial []byte
			var sc graphics.Scanner
			var got []int
			var rebuilt []byte
			for i := 0; i < len(stream); i += size {
				end := min(i+size, len(stream))
				data := stream[i:end]
				if len(partial) > 0 {
					data = append(partial, data...)
					partial = nil
				}
				if held := ansi.PartialTailLen(data); held > 0 && held <= maxHeldTail {
					partial = append([]byte(nil), data[len(data)-held:]...)
					data = data[:len(data)-held]
				}
				if len(data) == 0 {
					continue
				}
				for _, seg := range sc.Scan(data) {
					if seg.Graphics {
						got = append(got, len(seg.Cmd.Payload))
						rebuilt = append(rebuilt, seg.Cmd.Raw...)
					} else {
						rebuilt = append(rebuilt, seg.Data...)
					}
				}
			}
			// What cm would forward, against what arrived. This is the comparison that says whether the
			// scanner under the holdback re-emits anything.
			if len(rebuilt) != len(stream) {
				t.Errorf("rebuilt %d bytes from %d in: a command was emitted more than once or dropped",
					len(rebuilt), len(stream))
			}
			if len(got) != len(want) {
				t.Fatalf("scanned %d commands, want %d: payloads %v", len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("command %d carries %d payload bytes, want %d: the holdback and the graphics "+
						"scanner disagree about where a command ends. all payloads %v",
						i, got[i], want[i], got)
				}
			}
		})
	}
}

func itoaLocal(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
