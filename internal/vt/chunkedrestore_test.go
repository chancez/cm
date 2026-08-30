package vt

import (
	"encoding/base64"
	"testing"

	"github.com/chancez/cm/internal/graphics"
)

// A re-emitted image large enough to be split into several commands must still end up on screen.
//
// This is the size every real image has. The reported case measured restore_bytes=2483667 for one
// screenshot against 28400 for a small image, and the small one displayed while the large one did not, so
// chunking is the only thing that differs between them. A fixture that fits in one command tests the wrong
// path.
//
// The oracle is a fresh model rather than the bytes: a chunked re-emission can parse cleanly, reassemble to
// the right payload, and still leave the terminal with no image, which is exactly what a user sees.
func TestAChunkedRetransmissionStillDraws(t *testing.T) {
	// Incompressible, so the base64 stays over the chunk threshold.
	raw := make([]byte, 200*1024*3)
	for i := range raw {
		raw[i] = byte(i * 7 % 251)
	}
	// Geometry matching the byte count exactly: an oversized payload is rejected outright.
	const w, h = 800, 256
	px := raw[:w*h*3]
	enc := base64.RawStdEncoding.EncodeToString(px)
	if len(enc) <= graphics.MaxCommandPayload {
		t.Fatalf("payload is %d bytes, one command's worth: this would not exercise chunking", len(enc))
	}

	store := graphics.NewStore(0)
	var sc graphics.Scanner
	add := func(cmd string) {
		t.Helper()
		for _, seg := range sc.Scan([]byte(cmd)) {
			if seg.Graphics {
				store.Add(seg.Cmd)
			}
		}
	}
	// Stored the way a program sends it: geometry on the first chunk, payload alone after.
	const step = 4096
	first := true
	for i := 0; i < len(enc); i += step {
		end := min(i+step, len(enc))
		more := "1"
		if end == len(enc) {
			more = "0"
		}
		control := "a=T,q=2,m=" + more
		if first {
			control = "a=T,q=2,f=24,s=800,v=256,i=7,m=" + more
			first = false
		}
		add("\x1b_G" + control + ";" + enc[i:end] + "\x1b\\")
	}

	rt := store.Retransmissions()
	if len(rt) != 1 {
		t.Fatalf("Retransmissions() returned %d, want 1", len(rt))
	}

	term, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer term.Close()
	if err := term.Resize(24, 80, 10, 20); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	// The restore's own order: screen-ish clear, then the transmission, then the placement.
	if err := term.Write([]byte("\x1b[2J\x1b[H")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := term.Write(rt[0].Bytes); err != nil {
		t.Fatalf("writing the retransmission error = %v", err)
	}
	if err := term.Write([]byte("\x1b_Ga=p,i=7,C=1,q=2\x1b\\")); err != nil {
		t.Fatalf("writing the placement error = %v", err)
	}

	got, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	if len(got) == 0 {
		t.Errorf("a chunked re-emission left no image on screen, so the placement draws nothing: "+
			"%d bytes in %d commands", len(rt[0].Bytes), 1+len(enc)/graphics.MaxCommandPayload)
	}
}
