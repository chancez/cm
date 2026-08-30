package graphics

import (
	"encoding/base64"
	"testing"
)

// A chunked transmission split at pty-read boundaries must come back as the commands that went in.
//
// The reads a pty delivers are far smaller than one graphics command: 1022 bytes on darwin against a 4096
// byte chunk, so every command spans several arrivals and a whole image spans hundreds. Losing one command
// boundary merges two chunks into a single command whose payload is longer than its geometry allows, and a
// terminal that decodes it rejects the image, so nothing is drawn and nothing says why.
func TestScannerKeepsCommandBoundariesAcrossPtySizedReads(t *testing.T) {
	const w, h = 200, 120
	px := make([]byte, w*h*3)
	for i := range px {
		px[i] = byte(i % 251)
	}
	enc := base64.RawStdEncoding.EncodeToString(px)

	// Built the way kitty's client builds it: geometry on the first chunk, payload alone after.
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
		first = false
	}

	// Every read size around a pty's, so this does not depend on one lucky alignment.
	for _, size := range []int{1, 2, 3, 7, 64, 511, 1021, 1022, 1023, 4095, 4096, 4097} {
		t.Run("reads of "+itoa(size), func(t *testing.T) {
			var sc Scanner
			var got []int
			var terminators int
			for i := 0; i < len(stream); i += size {
				end := min(i+size, len(stream))
				for _, seg := range sc.Scan(stream[i:end]) {
					if !seg.Graphics {
						continue
					}
					got = append(got, len(seg.Cmd.Payload))
					if !seg.Cmd.More {
						terminators++
					}
				}
			}
			if len(got) != len(want) {
				t.Fatalf("scanned %d commands, want %d (payloads %v)", len(got), len(want), got)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("command %d carries %d payload bytes, want %d: a boundary was lost and two "+
						"chunks were merged. all payloads %v", i, got[i], want[i], got)
				}
			}
			if terminators != 1 {
				t.Errorf("%d commands ended the transmission, want exactly 1", terminators)
			}
		})
	}
}

func itoa(n int) string {
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
