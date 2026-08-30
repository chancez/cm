package vt

import (
	"encoding/base64"
	"testing"
)

// The emulator property cm's restore order rests on: erasing the display discards a stored image, not
// merely the placements on the cells it erases.
//
// This is why a restore sends the screen first and the image transmissions after it. Sending them ahead
// looks right, since an id has to exist before a placement names it, but the screen begins by clearing,
// and that clear takes the image with it. The a=p that follows then names nothing and draws nothing, which
// reached a user as a second client attaching to a session with an image on screen, receiving every byte
// of it, and showing a blank space.
//
// Pinned here rather than assumed in a comment, because the whole ordering in Session.attach is derived
// from it and nothing else would notice if it changed: the bytes stay well formed either way, so the
// failure is invisible except as a missing picture.
func TestEraseDisplayDiscardsAStoredImage(t *testing.T) {
	// 20x20 RGB, small enough to transmit in one command.
	var px []byte
	for i := 0; i < 20*20; i++ {
		px = append(px, 1, 2, 3)
	}
	transmit := "\x1b_Ga=t,q=2,f=24,s=20,v=20,i=7;" + base64.RawStdEncoding.EncodeToString(px) + "\x1b\\"
	place := "\x1b_Ga=p,i=7,C=1,q=2\x1b\\"

	tests := []struct {
		name  string
		order []string
		want  int
	}{
		// The control: with nothing between them the image is placed, so a zero below means the sequence
		// in the middle did it rather than the fixture being wrong.
		{"transmit then place", []string{transmit, place}, 1},
		// A cursor move between them is harmless, which narrows it to the erase.
		{"cursor move between", []string{transmit, "\x1b[H", place}, 1},
		// The finding. ED2 between them and the placement resolves to nothing.
		{"erase display between", []string{transmit, "\x1b[2J", place}, 0},
		// And the order a restore therefore has to use.
		{"erase first, then transmit", []string{"\x1b[2J\x1b[H", transmit, place}, 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term, err := NewSessionTerminal(24, 80, 0)
			if err != nil {
				t.Fatalf("NewSessionTerminal() error = %v", err)
			}
			defer term.Close()
			// Cell metrics, without which every placement reports itself off-screen and this test would
			// read zero for the wrong reason.
			if err := term.Resize(24, 80, 10, 20); err != nil {
				t.Fatalf("Resize() error = %v", err)
			}
			for _, w := range tt.order {
				if err := term.Write([]byte(w)); err != nil {
					t.Fatalf("Write() error = %v", err)
				}
			}
			got, err := term.Placements()
			if err != nil {
				t.Fatalf("Placements() error = %v", err)
			}
			if len(got) != tt.want {
				t.Errorf("Placements() = %+v, want %d of them", got, tt.want)
			}
		})
	}
}
