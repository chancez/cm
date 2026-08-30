package vt

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// A PNG transmission is stored and placed, which needs the decoder libghostty asks embedders to install.
//
// Without it the model rejects every PNG and holds no image, and that is the case that matters rather
// than an edge one: `kitten icat` on a file sends f=100. cm re-transmitted such an image to an attaching
// client correctly and emitted no placement, because the model had nothing on its screen to place.
func TestGraphicsStoresAPNG(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	// A 3x2 PNG, small enough to assert on exactly.
	img := image.NewNRGBA(image.Rect(0, 0, 3, 2))
	for x := 0; x < 3; x++ {
		for y := 0; y < 2; y++ {
			img.Set(x, y, color.NRGBA{R: uint8(10 * x), G: uint8(20 * y), B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode() error = %v", err)
	}

	if err := term.Write([]byte(graphicsCommand("a=T,f=100,s=3,v=2,i=5", buf.Bytes()))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	stored, ok, err := term.ImageByID(5)
	if err != nil {
		t.Fatalf("ImageByID() error = %v", err)
	}
	if !ok {
		t.Fatal("no image stored for a PNG transmission, so the decoder is not installed")
	}
	// Decoded to RGBA at the size the PNG declared, which is what the header promises.
	want := Image{ID: 5, Width: 3, Height: 2, Format: FormatRGBA, DataLen: 3 * 2 * 4}
	if stored != want {
		t.Errorf("ImageByID() = %+v, want %+v", stored, want)
	}

	// And it is placed, so a restore has something to put back.
	places, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	if len(places) != 1 {
		t.Fatalf("Placements() returned %d, want 1: %+v", len(places), places)
	}
	if places[0].ImageID != 5 {
		t.Errorf("Placements()[0] = %+v, want image 5", places[0])
	}
}

// A placement reports where it is once the model knows its cell size, which is the whole point of
// carrying cell metrics into Resize.
//
// libghostty derives a placement's rows and columns from the cell size, so with zeros it reports every
// image off-screen and a restore has nothing to place. The values were never missing: a client's pixel
// size is on the wire and reaches the pty, and the server resized the model without it. This asserts the
// consequence rather than the plumbing, since that is what a restore depends on.
func TestPlacementPositionNeedsCellMetrics(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		cellWidth, cellHeight uint16
		wantOnScreen          bool
	}{
		{name: "with cell metrics", cellWidth: 10, cellHeight: 20, wantOnScreen: true},
		{name: "without", cellWidth: 0, cellHeight: 0, wantOnScreen: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, err := New(24, 80, Callbacks{})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer term.Close()
			if err := term.Resize(24, 80, tc.cellWidth, tc.cellHeight); err != nil {
				t.Fatalf("Resize() error = %v", err)
			}

			// An image two cells wide and one tall at those metrics, placed at the cursor, which is home.
			if err := term.Write([]byte(graphicsCommand("a=T,f=24,s=20,v=20,i=9", rgbPixels(20, 20)))); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			places, err := term.Placements()
			if err != nil {
				t.Fatalf("Placements() error = %v", err)
			}
			if len(places) != 1 {
				t.Fatalf("Placements() returned %d, want 1", len(places))
			}
			if places[0].OnScreen != tc.wantOnScreen {
				t.Errorf("OnScreen = %v, want %v: %+v", places[0].OnScreen, tc.wantOnScreen, places[0])
			}
			if tc.wantOnScreen && (places[0].Col != 0 || places[0].Row != 0) {
				t.Errorf("placement at col %d row %d, want 0,0 for an image drawn at home",
					places[0].Col, places[0].Row)
			}
		})
	}
}
