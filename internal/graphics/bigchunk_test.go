package graphics

import (
	"encoding/base64"
	"strings"
	"testing"
)

// A stored image larger than one command must be re-emitted as chunks a terminal can reassemble.
//
// The size that matters is a real one: a screenshot inlined as RGB runs to megabytes of base64, so every
// image a user actually displays takes this path while a small fixture takes the single-command path. A
// test with a tiny payload therefore proves nothing about what ships.
func TestStoreRetransmitsALargeImageAsValidChunks(t *testing.T) {
	// Big enough to need several commands. Uncompressed, so the size is predictable: a compressible
	// fixture shrinks below the chunk threshold and stops exercising this.
	raw := make([]byte, 300*1024)
	for i := range raw {
		raw[i] = byte(i * 7 % 251)
	}
	enc := base64.RawStdEncoding.EncodeToString(raw)
	if len(enc) <= MaxCommandPayload {
		t.Fatalf("payload is %d bytes, which fits in one command: this test would not exercise chunking",
			len(enc))
	}

	// Stored the way it arrives from a program: geometry on the first chunk, payload alone after it.
	s := NewStore(0)
	const step = 4096
	first := true
	for i := 0; i < len(enc); i += step {
		end := min(i+step, len(enc))
		more := end < len(enc)
		control := "a=T,q=2,m=" + boolKey(more)
		if first {
			control = "a=T,q=2,f=24,s=800,v=128,i=7,m=" + boolKey(more)
			first = false
		}
		s.Add(mustParse(t, "\x1b_G"+control+";"+enc[i:end]+"\x1b\\"))
	}

	got := s.Retransmissions()
	if len(got) != 1 {
		t.Fatalf("Retransmissions() returned %d, want 1", len(got))
	}

	// Every command the re-emission produced, parsed back the way a terminal reads them.
	var sc Scanner
	segs := sc.Scan(got[0].Bytes)
	var cmds []Command
	for _, seg := range segs {
		if seg.Graphics {
			cmds = append(cmds, seg.Cmd)
		}
	}
	if len(cmds) < 2 {
		t.Fatalf("re-emission is %d command(s), want it chunked", len(cmds))
	}

	// The first carries the geometry, or the terminal cannot decode the image.
	for _, want := range []string{"s=800", "v=128", "f=24", "i=7"} {
		if !strings.Contains(cmds[0].Control, want) {
			t.Errorf("first chunk's control %q is missing %q", cmds[0].Control, want)
		}
	}
	// Exactly the last one ends the transmission, or the image never completes.
	for i, c := range cmds {
		wantMore := i < len(cmds)-1
		if c.More != wantMore {
			t.Errorf("chunk %d of %d has More=%v, want %v (control %q)",
				i, len(cmds), c.More, wantMore, c.Control)
		}
	}
	// And the payload reassembles to what was stored, with no chunk split inside a base64 quantum:
	// a receiver concatenates before decoding, so a split at a non-multiple of four corrupts the image.
	var rebuilt []byte
	for i, c := range cmds {
		if i < len(cmds)-1 && len(c.Payload)%4 != 0 {
			t.Errorf("chunk %d payload is %d bytes, not a multiple of four", i, len(c.Payload))
		}
		rebuilt = append(rebuilt, c.Payload...)
	}
	if string(rebuilt) != enc {
		t.Errorf("reassembled payload is %d bytes, want the stored %d", len(rebuilt), len(enc))
	}
}
