//go:build cgo

package vt

import (
	"encoding/base64"
	"fmt"
	"testing"
)

// What re-transmitting a stored image costs, which is the number that decides whether restoring images
// on reattach is viable at all.
//
// The asymmetry is the whole question. A program sends an image compressed: the captured `kitten icat`
// run transmitted a 1712x1294 image as 131109 bytes of chunked base64 PNG. libghostty stores what that
// decodes to, and hands back only the decoded pixels, so cm cannot replay what arrived. Re-emitting
// means base64 of raw RGBA, and RGBA of that geometry is 1712*1294*4 = 8862752 bytes before base64,
// which is 11817008 after. That is 90x what came in, and it is what a reattach would have to write to
// the client's terminal.
//
// Measured rather than computed, because the numbers decide the design and a formula would not catch
// libghostty storing something other than what was asked for.

// realWorldImage is the geometry from the captured icat run, so the numbers describe a real case rather
// than a chosen one.
const (
	realWorldWidth  = 1712
	realWorldHeight = 1294
	// realWorldWireBytes is what the whole transmission occupied on the pty, base64 PNG included.
	realWorldWireBytes = 131109
)

// TestGraphicsRetransmitCost reports the ratio between what arrives and what re-emission would send.
//
// Not an assertion about a threshold, because there is no correct number to assert: the point is to put
// the measurement in the record so the design decision has one. It fails only if the stored size differs
// from the geometry, which would mean the format assumption is wrong.
func TestGraphicsRetransmitCost(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	// A 10 MB default cannot hold an RGBA image this size, so the limit is raised for the measurement.
	// That is itself a finding: the default is below one full-screen image.
	if err := term.SetGraphicsStorageLimit(64 << 20); err != nil {
		t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}

	for _, tc := range []struct {
		name          string
		format        string
		bytesPerPixel int
		wantFormat    ImageFormat
	}{
		{"rgb", "24", 3, FormatRGB},
		{"rgba", "32", 4, FormatRGBA},
	} {
		t.Run(tc.name, func(t *testing.T) {
			term, err := New(24, 80, Callbacks{})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			defer term.Close()
			if err := term.SetGraphicsStorageLimit(64 << 20); err != nil {
				t.Fatalf("SetGraphicsStorageLimit() error = %v", err)
			}

			raw := make([]byte, realWorldWidth*realWorldHeight*tc.bytesPerPixel)
			for i := range raw {
				raw[i] = byte(i)
			}
			cmd := graphicsCommand(fmt.Sprintf("a=T,f=%s,s=%d,v=%d,i=1",
				tc.format, realWorldWidth, realWorldHeight), raw)

			if err := term.Write([]byte(cmd)); err != nil {
				t.Fatalf("Write() error = %v", err)
			}

			img, ok, err := term.ImageByID(1)
			if err != nil {
				t.Fatalf("ImageByID() error = %v", err)
			}
			if !ok {
				t.Fatal("the image was not stored; raise the storage limit or check the format")
			}
			if img.Format != tc.wantFormat {
				t.Errorf("stored format = %v, want %v", img.Format, tc.wantFormat)
			}
			wantLen := realWorldWidth * realWorldHeight * tc.bytesPerPixel
			if img.DataLen != wantLen {
				t.Errorf("stored DataLen = %d, want %d; the cost numbers below assume this",
					img.DataLen, wantLen)
			}

			pixels, ok, err := term.ImagePixels(1)
			if err != nil {
				t.Fatalf("ImagePixels() error = %v", err)
			}
			if !ok {
				t.Fatal("ImagePixels reported no image")
			}

			// What cm would have to write to a client to restore this one image.
			encoded := base64.StdEncoding.EncodedLen(len(pixels))
			t.Logf("%dx%d %s: stored %d bytes, base64 %d bytes",
				realWorldWidth, realWorldHeight, tc.name, len(pixels), encoded)
			t.Logf("  against %d bytes on the wire inbound: %.1fx larger outbound",
				realWorldWireBytes, float64(encoded)/float64(realWorldWireBytes))
			t.Logf("  the 10MB default limit holds %.1f images of this size",
				float64(defaultGraphicsStorageLimit)/float64(len(pixels)))
		})
	}
}

// BenchmarkGraphicsRetransmit measures the cost of producing the bytes for one image, which is what a
// reattach would pay per image on the screen.
//
// Split so the two halves are separable: copying the pixels out of libghostty, and base64 encoding them.
// If restore is too slow, this says which half to attack.
func BenchmarkGraphicsRetransmit(b *testing.B) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		b.Fatalf("New() error = %v", err)
	}
	defer term.Close()
	if err := term.SetGraphicsStorageLimit(64 << 20); err != nil {
		b.Fatalf("SetGraphicsStorageLimit() error = %v", err)
	}

	raw := make([]byte, realWorldWidth*realWorldHeight*4)
	cmd := graphicsCommand(fmt.Sprintf("a=T,f=32,s=%d,v=%d,i=1",
		realWorldWidth, realWorldHeight), raw)
	if err := term.Write([]byte(cmd)); err != nil {
		b.Fatalf("Write() error = %v", err)
	}

	b.Run("copy-pixels", func(b *testing.B) {
		b.SetBytes(int64(len(raw)))
		b.ResetTimer()
		for range b.N {
			if _, ok, err := term.ImagePixels(1); err != nil || !ok {
				b.Fatalf("ImagePixels() ok=%v err=%v", ok, err)
			}
		}
	})

	pixels, _, err := term.ImagePixels(1)
	if err != nil {
		b.Fatalf("ImagePixels() error = %v", err)
	}
	b.Run("base64-encode", func(b *testing.B) {
		dst := make([]byte, base64.StdEncoding.EncodedLen(len(pixels)))
		b.SetBytes(int64(len(pixels)))
		b.ResetTimer()
		for range b.N {
			base64.StdEncoding.Encode(dst, pixels)
		}
	})
}
