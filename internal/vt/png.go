package vt

/*
// The #cgo CFLAGS and LDFLAGS live in vt.go and apply to the whole package.
#include <ghostty/vt.h>
#include <stdlib.h>
#include "callbacks.h"
*/
import "C"

import (
	"bytes"
	"image"
	"image/draw"
	"image/png"
	"sync"
	"unsafe"
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
	installPNGOnce.Do(func() { C.cm_install_png_decoder() })
}

var installPNGOnce sync.Once

// cmDecodePNG decodes a PNG payload into the RGBA buffer libghostty expects.
//
// The output buffer must come from libghostty's own allocator, because the library takes ownership and
// frees it with the same one. Allocating with Go's memory and handing over the pointer would be freed by
// a C allocator, which is a crash rather than a leak.
//
//export cmDecodePNG
func cmDecodePNG(
	_ unsafe.Pointer,
	allocator *C.GhosttyAllocator,
	data *C.uint8_t,
	dataLen C.size_t,
	out *C.GhosttySysImage,
) C.bool {
	// A copy, because the decoder reads it after this returns nothing but the pointer is only valid for
	// the call. GoBytes copies.
	raw := C.GoBytes(unsafe.Pointer(data), C.int(dataLen))

	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		// False rather than a panic: a malformed PNG is a program's mistake, and libghostty answers the
		// program with a protocol error. A panic here would cross the cgo boundary and end the server.
		return C.bool(false)
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

	buf := C.ghostty_alloc(allocator, C.size_t(len(pix)))
	if buf == nil {
		return C.bool(false)
	}
	copy(unsafe.Slice((*byte)(unsafe.Pointer(buf)), len(pix)), pix)

	out.width = C.uint32_t(b.Dx())
	out.height = C.uint32_t(b.Dy())
	out.data = buf
	out.data_len = C.size_t(len(pix))
	return C.bool(true)
}
