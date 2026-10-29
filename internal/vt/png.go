//go:build cgo

package vt

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"sync"

	"go.mitchellh.com/libghostty"
)

// InstallPNGDecoder teaches libghostty to accept PNG images, once per process.
//
// Without it every PNG transmission is rejected by the terminal model, and that is not a cosmetic gap:
// `kitten icat` on a file sends f=100, PNG, so the model held no image and reported no placement for
// exactly the images a person is most likely to display. Measured against the reported command: cm
// re-transmitted the payload to an attaching client correctly and emitted no placement at all, because
// it had nothing to place. The image data was fine; the model's idea of the screen was empty.
//
// Process-wide because libghostty's sys options are, so this is a package-level install rather than a
// per-terminal one. Idempotent, since a second call would set the same pointer and every session start
// would otherwise race the first.
//
// The decode itself is Go's image/png, which cm already links transitively and which needs no C
// dependency. The cost lands on the pump goroutine that fed the transmission, once per image.
func InstallPNGDecoder() {
	installPNGOnce.Do(func() { _ = libghostty.SysSetDecodePng(decodePNG) })
}

var installPNGOnce sync.Once

// decodePNG decodes a PNG payload into the RGBA pixels libghostty expects.
//
// The binding copies the pixels into memory libghostty's own allocator owns, which is the part that
// used to be easy to get wrong: the library frees that buffer itself, so handing it Go memory was a
// crash rather than a leak.
func decodePNG(data []byte) (*libghostty.SysImage, error) {
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		// An error rather than a panic: a malformed PNG is a program's mistake, and libghostty answers
		// the program with a protocol error. A panic here would cross the cgo boundary and end the
		// server.
		return nil, err
	}

	// Normalized to RGBA, which is the one layout the struct documents. png.Decode returns whatever the
	// file used, so a paletted or 16-bit image would otherwise be handed over with the wrong stride.
	b := img.Bounds()
	rgba, ok := img.(*image.NRGBA)
	var pix []byte
	if ok && b.Min.X == 0 && b.Min.Y == 0 {
		pix = rgba.Pix
	} else {
		conv := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
		draw.Draw(conv, conv.Bounds(), img, b.Min, draw.Src)
		pix = conv.Pix
	}
	return &libghostty.SysImage{Width: uint32(b.Dx()), Height: uint32(b.Dy()), Data: pix}, nil
}
