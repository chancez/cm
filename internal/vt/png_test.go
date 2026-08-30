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

// A placement has no resolved position until the model knows its cell size, and this records that gap
// rather than leaving it to be rediscovered.
//
// libghostty derives how many rows an image covers from the cell pixel dimensions passed to
// ghostty_terminal_resize, and cm passes 0, 0 because only a client knows them: they come from the pty's
// ws_xpixel and ws_ypixel, which cm's size protocol does not carry. So viewport_pos reports the
// placement as off-screen and a restore skips it. The image itself is retained and re-transmitted, so
// what is missing is only where to draw it.
//
// Fixing it means carrying pixel dimensions from client to server, which is a wire change. When that
// lands, this test should be replaced by one asserting a real position.
func TestPlacementHasNoPositionWithoutCellMetrics(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.Write([]byte(graphicsCommand("a=T,f=24,s=2,v=2,i=9", rgbPixels(2, 2)))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	places, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	if len(places) != 1 {
		t.Fatalf("Placements() returned %d, want 1", len(places))
	}
	if places[0].OnScreen {
		t.Errorf("Placements()[0].OnScreen = true, want false until cm passes cell dimensions: %+v",
			places[0])
	}
}

// A malformed PNG is refused rather than crashing the process, since the decode runs inside a cgo
// callback where a panic would cross the boundary and end the server.
func TestGraphicsRejectsAMalformedPNG(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.Write([]byte(graphicsCommand("a=T,f=100,s=3,v=2,i=6", []byte("not a png")))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, ok, err := term.ImageByID(6); err != nil {
		t.Fatalf("ImageByID() error = %v", err)
	} else if ok {
		t.Error("a malformed PNG was stored, want it refused")
	}
}
