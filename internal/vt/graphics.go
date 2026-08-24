package vt

/*
// The #cgo CFLAGS and LDFLAGS for libghostty live in vt.go and apply to the whole package, so
// they are deliberately absent here. Repeating them named the same static archive twice on the
// link line, which ld reports as "ignoring duplicate libraries" on every build.
#include <ghostty/vt.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// ImageFormat is the pixel layout of a stored image.
//
// PNG is deliberately absent from the values a caller can observe. libghostty decodes a PNG payload
// before storing it and documents that the reported format is never PNG, so anything read back out is
// raw pixels. That is the single most important fact about this API: cm cannot recover the bytes a
// program sent, only the pixels they decoded to, and re-transmitting means re-encoding.
type ImageFormat int

const (
	// FormatRGB is three bytes per pixel.
	FormatRGB ImageFormat = iota
	// FormatRGBA is four bytes per pixel.
	FormatRGBA
	// FormatGrayAlpha is two bytes per pixel.
	FormatGrayAlpha
	// FormatGray is one byte per pixel.
	FormatGray
)

// String names the format for diagnostics.
func (f ImageFormat) String() string {
	switch f {
	case FormatRGB:
		return "rgb"
	case FormatRGBA:
		return "rgba"
	case FormatGrayAlpha:
		return "gray+alpha"
	case FormatGray:
		return "gray"
	}
	return fmt.Sprintf("unknown(%d)", int(f))
}

// BytesPerPixel is how many bytes one pixel occupies in this format.
func (f ImageFormat) BytesPerPixel() int {
	switch f {
	case FormatRGB:
		return 3
	case FormatRGBA:
		return 4
	case FormatGrayAlpha:
		return 2
	case FormatGray:
		return 1
	}
	return 0
}

// Image describes one image the terminal has stored.
//
// DataLen rather than the pixels themselves, because the pixel buffer is borrowed from the terminal and
// is invalidated by the next mutating call. A caller that needs the bytes asks for them explicitly with
// ImagePixels, which copies, so a borrowed pointer never escapes this package.
type Image struct {
	// ID is the image id the program assigned, which is how a placement refers to it.
	ID uint32
	// Number is the image number, an alternative addressing scheme programs may use instead of an id.
	Number uint32
	// Width and Height are in pixels.
	Width, Height uint32
	// Format is the pixel layout.
	Format ImageFormat
	// DataLen is the size of the stored pixel data in bytes, always Width*Height*BytesPerPixel.
	DataLen int
}

// Placement is one drawing of an image on the screen.
//
// An image can be placed more than once, so placements rather than images are what a screen consists
// of. The coordinates are the ones libghostty resolved, not the ones the program asked for.
type Placement struct {
	// ImageID names the image this draws.
	ImageID uint32
	// PlacementID distinguishes several placements of one image. Zero when the program gave none.
	PlacementID uint32
	// Virtual reports a placement positioned by unicode placeholders rather than by the cursor.
	Virtual bool
	// XOffset and YOffset are pixel offsets within the starting cell.
	XOffset, YOffset uint32
	// SourceX, SourceY, SourceWidth, SourceHeight crop the image. Zero width or height means the
	// whole image in that axis.
	SourceX, SourceY          uint32
	SourceWidth, SourceHeight uint32
	// Columns and Rows are how many cells the placement covers. Zero means libghostty derived it.
	Columns, Rows uint32
	// Z is the stacking order against text and other placements.
	Z int32
}

// GraphicsEnabled reports whether this terminal is storing kitty graphics images.
//
// False until SetGraphicsStorageLimit has been given a non-zero limit. Worth checking rather than
// assuming, because storage is opt-in: a terminal that has never been told a limit silently retains no
// images at all, and every query below then returns nothing for a session that really did display one.
func (t *Terminal) GraphicsEnabled() (bool, error) {
	limit, err := t.graphicsStorageLimit()
	if err != nil {
		return false, err
	}
	return limit > 0, nil
}

// graphicsStorageLimit reads back the configured storage limit in bytes.
func (t *Terminal) graphicsStorageLimit() (uint64, error) {
	var n C.size_t
	if err := check(
		C.ghostty_terminal_get(t.ptr, C.GHOSTTY_TERMINAL_DATA_KITTY_IMAGE_STORAGE_LIMIT,
			unsafe.Pointer(&n)),
		"reading kitty image storage limit",
	); err != nil {
		return 0, err
	}
	return uint64(n), nil
}

// SetGraphicsStorageLimit bounds the bytes of decoded image data this terminal retains.
//
// Zero disables storage, which is libghostty's default and therefore cm's behavior until this is
// called. A limit is required rather than optional: the stored form is decoded pixels, so an image is
// far larger in the terminal than it was on the wire, and an unbounded store on a long-lived session
// is a leak measured in screens of images rather than in bytes.
//
// Note this does not by itself make PNG payloads work. libghostty needs a decoder installed
// process-wide for those, and rejects them otherwise. RGB and RGBA payloads need no decoder.
func (t *Terminal) SetGraphicsStorageLimit(bytes int) error {
	if bytes <= 0 {
		return check(
			C.ghostty_terminal_set(t.ptr, C.GHOSTTY_TERMINAL_OPT_KITTY_IMAGE_STORAGE_LIMIT, nil),
			"clearing kitty image storage limit",
		)
	}
	n := C.size_t(bytes)
	// The address of a local is safe here for the same reason as SetScrollbackLimit: libghostty reads
	// the value during the call rather than retaining the pointer, unlike a callback option.
	return check(
		C.ghostty_terminal_set(t.ptr, C.GHOSTTY_TERMINAL_OPT_KITTY_IMAGE_STORAGE_LIMIT,
			unsafe.Pointer(&n)),
		"setting kitty image storage limit",
	)
}

// graphics borrows the terminal's image storage handle.
//
// The handle is invalidated by the next mutating terminal call, so it is fetched per operation rather
// than cached on the Terminal. That is not a performance concern here: everything using it runs on
// attach or when dumping state, never per byte of output.
func (t *Terminal) graphics() (C.GhosttyKittyGraphics, error) {
	var g C.GhosttyKittyGraphics
	if err := check(
		C.ghostty_terminal_get(t.ptr, C.GHOSTTY_TERMINAL_DATA_KITTY_GRAPHICS, unsafe.Pointer(&g)),
		"getting kitty graphics storage",
	); err != nil {
		return nil, err
	}
	if g == nil {
		return nil, fmt.Errorf("kitty graphics unavailable in this build")
	}
	return g, nil
}

// Placements lists every placement on the active screen, in iteration order.
//
// Empty when graphics storage is disabled or nothing has been drawn. The slice is fully owned by the
// caller: nothing in it borrows from the terminal, so it stays valid across later writes.
func (t *Terminal) Placements() ([]Placement, error) {
	if t.closed {
		return nil, errors.New("terminal is closed")
	}
	g, err := t.graphics()
	if err != nil {
		return nil, err
	}

	var it C.GhosttyKittyGraphicsPlacementIterator
	// A nil allocator asks libghostty for its default, which is libc malloc since cm links libc.
	if err := check(
		C.ghostty_kitty_graphics_placement_iterator_new(nil, &it),
		"creating a placement iterator",
	); err != nil {
		return nil, err
	}
	defer C.ghostty_kitty_graphics_placement_iterator_free(it)

	// Populating the iterator from the storage is a separate step from creating it, so one iterator can
	// be refilled rather than reallocated.
	if err := check(
		C.ghostty_kitty_graphics_get(g, C.GHOSTTY_KITTY_GRAPHICS_DATA_PLACEMENT_ITERATOR,
			unsafe.Pointer(&it)),
		"populating the placement iterator",
	); err != nil {
		return nil, err
	}

	var out []Placement
	for bool(C.ghostty_kitty_graphics_placement_next(it)) {
		p, err := placementAt(it)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// placementAt reads the iterator's current placement.
//
// Each field is fetched individually rather than through the _multi form. The multi call exists to
// amortize FFI overhead and would be worth using on a hot path; this runs on attach, and one call per
// field is far easier to keep correct against an API whose output types differ per key.
func placementAt(it C.GhosttyKittyGraphicsPlacementIterator) (Placement, error) {
	var p Placement

	u32 := func(kind C.GhosttyKittyGraphicsPlacementData, dst *uint32, what string) error {
		var v C.uint32_t
		if err := check(
			C.ghostty_kitty_graphics_placement_get(it, kind, unsafe.Pointer(&v)),
			"reading placement "+what,
		); err != nil {
			return err
		}
		*dst = uint32(v)
		return nil
	}

	for _, f := range []struct {
		kind C.GhosttyKittyGraphicsPlacementData
		dst  *uint32
		what string
	}{
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_IMAGE_ID, &p.ImageID, "image id"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_PLACEMENT_ID, &p.PlacementID, "placement id"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_X_OFFSET, &p.XOffset, "x offset"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_Y_OFFSET, &p.YOffset, "y offset"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_SOURCE_X, &p.SourceX, "source x"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_SOURCE_Y, &p.SourceY, "source y"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_SOURCE_WIDTH, &p.SourceWidth, "source width"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_SOURCE_HEIGHT, &p.SourceHeight, "source height"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_COLUMNS, &p.Columns, "columns"},
		{C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_ROWS, &p.Rows, "rows"},
	} {
		if err := u32(f.kind, f.dst, f.what); err != nil {
			return Placement{}, err
		}
	}

	var virtual C.bool
	if err := check(
		C.ghostty_kitty_graphics_placement_get(it,
			C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_IS_VIRTUAL, unsafe.Pointer(&virtual)),
		"reading placement virtual flag",
	); err != nil {
		return Placement{}, err
	}
	p.Virtual = bool(virtual)

	var z C.int32_t
	if err := check(
		C.ghostty_kitty_graphics_placement_get(it,
			C.GHOSTTY_KITTY_GRAPHICS_PLACEMENT_DATA_Z, unsafe.Pointer(&z)),
		"reading placement z",
	); err != nil {
		return Placement{}, err
	}
	p.Z = int32(z)

	return p, nil
}

// ImageByID describes a stored image, reporting ok=false when no image has that id.
//
// A missing image is an ordinary outcome rather than an error: a placement can outlive the image it
// refers to when storage evicts under its limit, and a caller walking placements has to cope with that
// rather than fail.
func (t *Terminal) ImageByID(id uint32) (img Image, ok bool, err error) {
	if t.closed {
		return Image{}, false, errors.New("terminal is closed")
	}
	g, err := t.graphics()
	if err != nil {
		return Image{}, false, err
	}

	h := C.ghostty_kitty_graphics_image(g, C.uint32_t(id))
	if h == nil {
		return Image{}, false, nil
	}

	u32 := func(kind C.GhosttyKittyGraphicsImageData, dst *uint32, what string) error {
		var v C.uint32_t
		if err := check(
			C.ghostty_kitty_graphics_image_get(h, kind, unsafe.Pointer(&v)),
			"reading image "+what,
		); err != nil {
			return err
		}
		*dst = uint32(v)
		return nil
	}
	for _, f := range []struct {
		kind C.GhosttyKittyGraphicsImageData
		dst  *uint32
		what string
	}{
		{C.GHOSTTY_KITTY_IMAGE_DATA_ID, &img.ID, "id"},
		{C.GHOSTTY_KITTY_IMAGE_DATA_NUMBER, &img.Number, "number"},
		{C.GHOSTTY_KITTY_IMAGE_DATA_WIDTH, &img.Width, "width"},
		{C.GHOSTTY_KITTY_IMAGE_DATA_HEIGHT, &img.Height, "height"},
	} {
		if err := u32(f.kind, f.dst, f.what); err != nil {
			return Image{}, false, err
		}
	}

	var format C.GhosttyKittyImageFormat
	if err := check(
		C.ghostty_kitty_graphics_image_get(h, C.GHOSTTY_KITTY_IMAGE_DATA_FORMAT,
			unsafe.Pointer(&format)),
		"reading image format",
	); err != nil {
		return Image{}, false, err
	}
	switch format {
	case C.GHOSTTY_KITTY_IMAGE_FORMAT_RGB:
		img.Format = FormatRGB
	case C.GHOSTTY_KITTY_IMAGE_FORMAT_RGBA:
		img.Format = FormatRGBA
	case C.GHOSTTY_KITTY_IMAGE_FORMAT_GRAY_ALPHA:
		img.Format = FormatGrayAlpha
	case C.GHOSTTY_KITTY_IMAGE_FORMAT_GRAY:
		img.Format = FormatGray
	default:
		// PNG is documented as impossible here, so seeing it means the header's guarantee changed and
		// silently treating it as pixels would hand out a buffer of the wrong size.
		return Image{}, false, fmt.Errorf("unexpected stored image format %d", int(format))
	}

	var dataLen C.size_t
	if err := check(
		C.ghostty_kitty_graphics_image_get(h, C.GHOSTTY_KITTY_IMAGE_DATA_DATA_LEN,
			unsafe.Pointer(&dataLen)),
		"reading image data length",
	); err != nil {
		return Image{}, false, err
	}
	img.DataLen = int(dataLen)

	return img, true, nil
}

// ImagePixels copies a stored image's pixel data out.
//
// A copy rather than a view, deliberately. The pointer libghostty hands back is borrowed and is
// invalidated by the next mutating terminal call, and the pump mutates the terminal on every chunk of
// output, so a slice aliasing that memory would be a use-after-free waiting for the next keystroke.
// Copying keeps the unsafe lifetime inside this function.
//
// Reports ok=false when no image has that id, matching ImageByID.
func (t *Terminal) ImagePixels(id uint32) (pixels []byte, ok bool, err error) {
	if t.closed {
		return nil, false, errors.New("terminal is closed")
	}
	g, err := t.graphics()
	if err != nil {
		return nil, false, err
	}

	h := C.ghostty_kitty_graphics_image(g, C.uint32_t(id))
	if h == nil {
		return nil, false, nil
	}

	var dataLen C.size_t
	if err := check(
		C.ghostty_kitty_graphics_image_get(h, C.GHOSTTY_KITTY_IMAGE_DATA_DATA_LEN,
			unsafe.Pointer(&dataLen)),
		"reading image data length",
	); err != nil {
		return nil, false, err
	}
	if dataLen == 0 {
		return nil, true, nil
	}

	var ptr unsafe.Pointer
	if err := check(
		C.ghostty_kitty_graphics_image_get(h, C.GHOSTTY_KITTY_IMAGE_DATA_DATA_PTR,
			unsafe.Pointer(&ptr)),
		"reading image data pointer",
	); err != nil {
		return nil, false, err
	}
	if ptr == nil {
		return nil, true, nil
	}

	return C.GoBytes(ptr, C.int(dataLen)), true, nil
}
