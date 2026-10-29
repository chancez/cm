//go:build cgo

package vt

import (
	"errors"
	"fmt"

	"go.mitchellh.com/libghostty"
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
	// Col and Row are where the placement's top-left corner sits in the viewport, zero-based.
	//
	// Row can be negative, for an image whose top has scrolled above the viewport while its lower
	// rows are still visible. A caller re-emitting a placement has to clamp that itself: a cursor
	// cannot be positioned above row zero, so the image would have to be cropped with a source
	// rectangle to land correctly.
	//
	// Only meaningful when OnScreen is true.
	Col, Row int32
	// OnScreen reports whether the placement is at least partly visible in the viewport.
	//
	// False for one that has scrolled entirely away and for a virtual placement, which has no cursor
	// position at all because unicode placeholders put it where the cells are. Both are ordinary
	// outcomes rather than errors, which is why this is a bool and not an error return: a screen
	// holding a placement that scrolled off is normal, and a caller restoring a screen wants to skip
	// it rather than fail.
	OnScreen bool
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
	n, err := t.term.KittyImageStorageLimit()
	return n, check(err, "reading kitty image storage limit")
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
		return check(t.term.SetKittyImageStorageLimit(nil), "clearing kitty image storage limit")
	}
	return check(t.term.SetKittyImageStorageLimit(new(uint64(bytes))), "setting kitty image storage limit")
}

// graphics borrows the terminal's image storage handle.
//
// The handle is invalidated by the next mutating terminal call, so it is fetched per operation rather
// than cached on the Terminal. That is not a performance concern here: everything using it runs on
// attach or when dumping state, never per byte of output.
func (t *Terminal) graphics() (*libghostty.KittyGraphics, error) {
	g, err := t.term.KittyGraphics()
	if err != nil {
		return nil, check(err, "getting kitty graphics storage")
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

	it, err := libghostty.NewKittyGraphicsPlacementIterator()
	if err != nil {
		return nil, check(err, "creating a placement iterator")
	}
	defer it.Close()

	// Populating the iterator from the storage is a separate step from creating it, so one iterator can
	// be refilled rather than reallocated.
	if err := g.PlacementIterator(it); err != nil {
		return nil, check(err, "populating the placement iterator")
	}

	var out []Placement
	for it.Next() {
		p, err := placementAt(it)
		if err != nil {
			return nil, err
		}
		if err := t.placementPos(it, g, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// placementPos fills in where a placement sits in the viewport.
//
// Separate from placementAt because it needs three handles rather than the iterator alone: the
// position lives in the placement's pin, and libghostty resolves it against the image's geometry and
// the terminal's viewport.
//
// This is the whole reason a restore can put an image back where it belongs. Without it cm knew what
// was placed but not where, so a re-emission had to use a=T and display at whatever the cursor
// happened to be, which is how a restored image landed on top of a full-screen program instead of in
// its own row.
func (t *Terminal) placementPos(it *libghostty.KittyGraphicsPlacementIterator, g *libghostty.KittyGraphics, p *Placement) error {
	// A virtual placement has no cursor position at all, so libghostty declines it and there is
	// nothing to ask for.
	if p.Virtual {
		return nil
	}
	img := g.Image(p.ImageID)
	if img == nil {
		// A placement whose image storage evicted. Nothing to place, and the caller already has to
		// cope with this: ImageByID documents the same outcome.
		return nil
	}

	col, row, err := it.ViewportPos(img, t.term)
	if errors.Is(err, libghostty.ErrNoValue) {
		// Scrolled entirely out of the viewport, or virtual. Not an error: a screen holding one is
		// normal and a caller restoring a screen wants to skip it.
		return nil
	}
	if err != nil {
		return check(err, "reading placement viewport position")
	}
	p.Col, p.Row, p.OnScreen = col, row, true
	return nil
}

// placementAt reads the iterator's current placement.
func placementAt(it *libghostty.KittyGraphicsPlacementIterator) (Placement, error) {
	var p Placement
	for _, f := range []struct {
		get  func() (uint32, error)
		dst  *uint32
		what string
	}{
		{it.ImageID, &p.ImageID, "image id"},
		{it.PlacementID, &p.PlacementID, "placement id"},
		{it.XOffset, &p.XOffset, "x offset"},
		{it.YOffset, &p.YOffset, "y offset"},
		{it.SourceX, &p.SourceX, "source x"},
		{it.SourceY, &p.SourceY, "source y"},
		{it.SourceWidth, &p.SourceWidth, "source width"},
		{it.SourceHeight, &p.SourceHeight, "source height"},
		{it.Columns, &p.Columns, "columns"},
		{it.Rows, &p.Rows, "rows"},
	} {
		v, err := f.get()
		if err != nil {
			return Placement{}, check(err, "reading placement "+f.what)
		}
		*f.dst = v
	}

	virtual, err := it.IsVirtual()
	if err != nil {
		return Placement{}, check(err, "reading placement virtual flag")
	}
	p.Virtual = virtual

	z, err := it.Z()
	if err != nil {
		return Placement{}, check(err, "reading placement z")
	}
	p.Z = z
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
	h := g.Image(id)
	if h == nil {
		return Image{}, false, nil
	}

	for _, f := range []struct {
		get  func() (uint32, error)
		dst  *uint32
		what string
	}{
		{h.ID, &img.ID, "id"},
		{h.Number, &img.Number, "number"},
		{h.Width, &img.Width, "width"},
		{h.Height, &img.Height, "height"},
	} {
		v, err := f.get()
		if err != nil {
			return Image{}, false, check(err, "reading image "+f.what)
		}
		*f.dst = v
	}

	format, err := h.Format()
	if err != nil {
		return Image{}, false, check(err, "reading image format")
	}
	switch format {
	case libghostty.KittyImageFormatRGB:
		img.Format = FormatRGB
	case libghostty.KittyImageFormatRGBA:
		img.Format = FormatRGBA
	case libghostty.KittyImageFormatGrayAlpha:
		img.Format = FormatGrayAlpha
	case libghostty.KittyImageFormatGray:
		img.Format = FormatGray
	default:
		// PNG is documented as impossible here, so seeing it means the header's guarantee changed and
		// silently treating it as pixels would hand out a buffer of the wrong size.
		return Image{}, false, fmt.Errorf("unexpected stored image format %d", int(format))
	}

	dataLen, err := h.DataLen()
	if err != nil {
		return Image{}, false, check(err, "reading image data length")
	}
	img.DataLen = int(dataLen)
	return img, true, nil
}

// ImagePixels copies a stored image's pixel data out.
//
// A copy rather than a view, deliberately. The slice the binding hands back is borrowed and is
// invalidated by the next mutating terminal call, and the pump mutates the terminal on every chunk of
// output, so a slice aliasing that memory would be a use-after-free waiting for the next keystroke.
// Copying keeps the borrowed lifetime inside this function.
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
	h := g.Image(id)
	if h == nil {
		return nil, false, nil
	}
	data, err := h.Data()
	if errors.Is(err, libghostty.ErrNoValue) {
		// Pending: the transmission has not completed, so there are no pixels yet.
		return nil, true, nil
	}
	if err != nil {
		return nil, false, check(err, "reading image data")
	}
	if len(data) == 0 {
		return nil, true, nil
	}
	return append([]byte(nil), data...), true, nil
}
