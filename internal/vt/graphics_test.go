//go:build cgo

package vt

import (
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// graphicsCommand builds a kitty graphics APC: ESC _ G <control> ; <base64 payload> ESC \.
func graphicsCommand(control string, payload []byte) string {
	return "\x1b_G" + control + ";" + base64.StdEncoding.EncodeToString(payload) + "\x1b\\"
}

// rgbPixels builds w*h pixels of RGB data, which needs no decoder installed.
func rgbPixels(w, h int) []byte {
	out := make([]byte, 0, w*h*3)
	for i := range w * h {
		out = append(out, byte(i), byte(i>>8), byte(i>>16))
	}
	return out
}

// defaultGraphicsStorageLimit is what libghostty starts a terminal with, measured rather than assumed.
//
// This corrects a belief worth naming, because it changed the plan: the header describes storage as
// something to enable by setting a non-zero limit, which reads as "off until asked for", and an earlier
// version of this file asserted exactly that and failed. The default is 10 MB, so **cm has been storing
// images since it first linked this library**, without asking and without using them. What was missing
// was never the storage, only the code to read it back.
//
// The number matters for the same reason: 10 MB of decoded pixels is about two 1712x1294 RGBA images, so
// the default is already a real per-session ceiling that scrollback-heavy use will hit.
const defaultGraphicsStorageLimit = 10_000_000

// The default is a real, non-zero limit, and an image is retained without cm doing anything.
func TestGraphicsStorageDefaultsToOn(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	limit, err := term.graphicsStorageLimit()
	if err != nil {
		t.Fatalf("graphicsStorageLimit() error = %v", err)
	}
	if limit != defaultGraphicsStorageLimit {
		t.Errorf("default storage limit = %d, want %d. If upstream changed this, the comment above and "+
			"the sizing in docs need the new number rather than a bare fix here.",
			limit, defaultGraphicsStorageLimit)
	}

	enabled, err := term.GraphicsEnabled()
	if err != nil {
		t.Fatalf("GraphicsEnabled() error = %v", err)
	}
	if !enabled {
		t.Error("GraphicsEnabled() = false with the default limit, want true")
	}

	// So a transmission is retained with no configuration at all.
	if err := term.Write([]byte(graphicsCommand("a=T,f=24,s=2,v=2,i=1", rgbPixels(2, 2)))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, ok, err := term.ImageByID(1); err != nil {
		t.Fatalf("ImageByID() error = %v", err)
	} else if !ok {
		t.Error("no image was stored under the default limit, want the one transmitted")
	}
}

// Setting the limit to zero really does disable storage, which is what makes it a usable control.
func TestGraphicsStorageCanBeDisabled(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.SetGraphicsStorageLimit(0); err != nil {
		t.Fatalf("SetGraphicsStorageLimit(0) error = %v", err)
	}
	enabled, err := term.GraphicsEnabled()
	if err != nil {
		t.Fatalf("GraphicsEnabled() error = %v", err)
	}
	if enabled {
		t.Error("GraphicsEnabled() = true after setting the limit to zero, want false")
	}

	if err := term.Write([]byte(graphicsCommand("a=T,f=24,s=2,v=2,i=1", rgbPixels(2, 2)))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if _, ok, err := term.ImageByID(1); err != nil {
		t.Fatalf("ImageByID() error = %v", err)
	} else if ok {
		t.Error("an image was stored with the limit at zero, want none")
	}
}

// With a limit set, an RGB transmission is stored and reads back with the geometry it was sent with.
//
// RGB rather than PNG on purpose: PNG needs a decoder installed process-wide, so using it here would
// conflate "the wrapper works" with "the decoder is installed" and fail for the wrong reason.
func TestGraphicsStoresAndReportsAnImage(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.SetGraphicsStorageLimit(1 << 20); err != nil {
		t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}
	enabled, err := term.GraphicsEnabled()
	if err != nil {
		t.Fatalf("GraphicsEnabled() error = %v", err)
	}
	if !enabled {
		t.Fatal("GraphicsEnabled() = false after setting a limit, want true")
	}

	const w, h = 4, 3
	pixels := rgbPixels(w, h)
	if err := term.Write([]byte(graphicsCommand(
		fmt.Sprintf("a=T,f=24,s=%d,v=%d,i=7", w, h), pixels))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, ok, err := term.ImageByID(7)
	if err != nil {
		t.Fatalf("ImageByID() error = %v", err)
	}
	if !ok {
		t.Fatal("ImageByID(7) reported no image, want the one just transmitted")
	}
	want := Image{
		ID:      7,
		Number:  0,
		Width:   w,
		Height:  h,
		Format:  FormatRGB,
		DataLen: w * h * 3,
	}
	if got != want {
		t.Errorf("ImageByID() = %+v, want %+v", got, want)
	}
}

// The pixels come back byte for byte, which is what a re-transmission would have to send.
func TestGraphicsPixelsRoundTrip(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.SetGraphicsStorageLimit(1 << 20); err != nil {
		t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}

	const w, h = 5, 4
	pixels := rgbPixels(w, h)
	if err := term.Write([]byte(graphicsCommand(
		fmt.Sprintf("a=T,f=24,s=%d,v=%d,i=3", w, h), pixels))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, ok, err := term.ImagePixels(3)
	if err != nil {
		t.Fatalf("ImagePixels() error = %v", err)
	}
	if !ok {
		t.Fatal("ImagePixels(3) reported no image")
	}
	if string(got) != string(pixels) {
		t.Errorf("ImagePixels() returned %d bytes, want the %d transmitted; first difference matters "+
			"because a re-transmission sends exactly these", len(got), len(pixels))
	}

	// The copy has to survive a later write, since the pump mutates the terminal on every chunk and a
	// slice aliasing libghostty's buffer would be freed underneath it.
	if err := term.Write([]byte("some ordinary output\r\n")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if string(got) != string(pixels) {
		t.Error("the pixel copy changed after a later terminal write, so it aliases libghostty's buffer")
	}
}

// A transmit-and-display command produces a placement, which is what a screen actually consists of.
func TestGraphicsReportsPlacements(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.SetGraphicsStorageLimit(1 << 20); err != nil {
		t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}

	if placements, err := term.Placements(); err != nil {
		t.Fatalf("Placements() error = %v", err)
	} else if len(placements) != 0 {
		t.Errorf("Placements() = %v on a fresh terminal, want none", placements)
	}

	const w, h = 2, 2
	if err := term.Write([]byte(graphicsCommand(
		fmt.Sprintf("a=T,f=24,s=%d,v=%d,i=11,p=5", w, h), rgbPixels(w, h)))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	placements, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	if len(placements) != 1 {
		t.Fatalf("Placements() returned %d placements, want 1: %+v", len(placements), placements)
	}
	if placements[0].ImageID != 11 {
		t.Errorf("placement ImageID = %d, want 11 (full placement: %+v)",
			placements[0].ImageID, placements[0])
	}
	if placements[0].PlacementID != 5 {
		t.Errorf("placement PlacementID = %d, want 5 (full placement: %+v)",
			placements[0].PlacementID, placements[0])
	}
}

// A placement referring to an image that is not stored has to be an ordinary outcome.
//
// Storage evicts under its limit while placements survive, so a caller walking placements will meet
// this and must not treat it as an error.
func TestGraphicsMissingImageIsNotAnError(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.SetGraphicsStorageLimit(1 << 20); err != nil {
		t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}

	if _, ok, err := term.ImageByID(999); err != nil {
		t.Errorf("ImageByID(999) error = %v, want a clean not-found", err)
	} else if ok {
		t.Error("ImageByID(999) reported an image that was never transmitted")
	}
	if _, ok, err := term.ImagePixels(999); err != nil {
		t.Errorf("ImagePixels(999) error = %v, want a clean not-found", err)
	} else if ok {
		t.Error("ImagePixels(999) reported an image that was never transmitted")
	}
}

// PNG is rejected rather than stored while no decoder is installed, and the wrapper must not report it
// as a stored image of some other format.
//
// This pins the boundary the next step moves: installing a decoder is what makes this case work, and
// until then a PNG payload has to fail visibly rather than appear as an empty or mis-sized image.
func TestGraphicsPNGWithoutDecoderIsNotStored(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.SetGraphicsStorageLimit(1 << 20); err != nil {
		t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}

	// A PNG header is enough: it is rejected before the pixel data matters.
	png := append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)
	if err := term.Write([]byte(graphicsCommand("a=T,f=100,i=21", png))); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	img, ok, err := term.ImageByID(21)
	if err != nil {
		// An error naming an unexpected format would mean the header's "never PNG" guarantee changed.
		if strings.Contains(err.Error(), "unexpected stored image format") {
			t.Fatalf("a PNG payload was stored in PNG form: %v", err)
		}
		t.Fatalf("ImageByID() error = %v", err)
	}
	if ok {
		t.Errorf("a PNG payload was stored without a decoder installed: %+v", img)
	}
}
