package graphics

import (
	"strconv"
)

// Placement is one image drawn on the screen, as the terminal model resolved it.
//
// Mirrors what internal/vt reports, kept as its own type so this package stays free of cgo and the
// re-emission can be tested without building a terminal.
type Placement struct {
	// ImageID names the image, which must already have been transmitted for a placement to resolve.
	ImageID uint32
	// PlacementID distinguishes several placements of one image. Zero when the program gave none.
	PlacementID uint32
	// Col and Row are the top-left corner in the viewport, zero-based. Row may be negative for an
	// image whose top has scrolled above the viewport.
	Col, Row int32
	// Columns and Rows are how many cells it covers, zero when the terminal derived them.
	Columns, Rows uint32
	// Z is the stacking order against text.
	Z int32
}

// PlaceCommands rebuilds the placements on a screen, to follow restored content.
//
// This is the half of an image restore that says *where*, and it exists because a transmission on its
// own says only *what*. cm used to re-emit stored images as a=T, transmit and display, which draws at
// whatever the cursor is when the bytes land: measured in a real kitty as an image placed on the main
// screen and then erased by the restore's own clear, and, with slightly different timing, as an image
// drawn on top of a full-screen program. A placement is a position, so it is restored as one.
//
// The shape of the output is load-bearing in three ways:
//
//   - It is wrapped in DECSC and DECRC (ESC 7 / ESC 8), so moving the cursor to each placement does not
//     disturb where the restored screen left it. Without that the cursor ends up at the last image.
//   - Every command carries C=1, which tells the terminal not to advance the cursor. Beyond tidiness
//     that prevents a placement on the last row from scrolling the whole screen up to make room.
//   - Every command carries q=2, for the reason every re-emission does: a reply to a command cm sent
//     would arrive on the input path answering a question cm never asked.
//
// A placement whose Row is negative has scrolled partly above the viewport, and it is cropped rather than
// skipped, because that case is the norm and not an edge. `kitten icat` scales an image to nearly fill the
// window, the shell then prints a prompt under it, and the scroll that causes puts the top of the image
// above the top of the screen: measured at row -1 after three newlines and -7 after nine. Skipping those
// meant a realistic image was never restored at all, while a tiny one was.
//
// Cropping is what libghostty's API asks of a caller: it reports the negative row and documents the
// embedder as responsible for clipping. The rows above the viewport are removed with a source offset,
// y= in pixels, which needs cellHeight to convert. The height is deliberately left unstated so the
// terminal uses the rest of the image, since kitty clips a source rectangle to the image's bounds.
//
// cellHeight of zero means the model was never told its cell size, so the offset cannot be computed and a
// scrolled placement is skipped as before. That is the only case where an image is silently dropped.
//
// Returns nil for no placements, which is the common case and costs nothing.
func PlaceCommands(placements []Placement, cellHeight uint16) []byte {
	var out []byte
	for _, p := range placements {
		if p.ImageID == 0 {
			continue
		}
		// How much of the image's top is off-screen, in pixels, and where the visible part starts.
		row, cropY := p.Row, 0
		if row < 0 {
			if cellHeight == 0 {
				continue
			}
			cropY = int(-row) * int(cellHeight)
			row = 0
		}
		if out == nil {
			out = append(out, "\x1b7"...) // DECSC, save cursor
		}
		// CUP is one-based, the placement is zero-based.
		out = append(out, "\x1b["...)
		out = append(out, strconv.Itoa(int(row)+1)...)
		out = append(out, ';')
		out = append(out, strconv.Itoa(int(p.Col)+1)...)
		out = append(out, 'H')

		control := "a=p,i=" + strconv.FormatUint(uint64(p.ImageID), 10)
		if p.PlacementID != 0 {
			control += ",p=" + strconv.FormatUint(uint64(p.PlacementID), 10)
		}
		// The cell extent is restated so the terminal scales the image the same way it did originally.
		// Absent means "derive from the image", which is right only if nothing about the geometry moved.
		if p.Columns != 0 {
			control += ",c=" + strconv.FormatUint(uint64(p.Columns), 10)
		}
		// Restated only when nothing was cropped: r= describes the uncropped image, and repeating it for a
		// cropped one would stretch the remainder back to the original height.
		if p.Rows != 0 && cropY == 0 {
			control += ",r=" + strconv.FormatUint(uint64(p.Rows), 10)
		}
		if p.Z != 0 {
			control += ",z=" + strconv.Itoa(int(p.Z))
		}
		// The rows that scrolled above the screen are cropped off the top of the source image, so what is
		// drawn lines up with what the restored screen shows.
		if cropY > 0 {
			control += ",y=" + strconv.Itoa(cropY)
		}
		control += ",C=1,q=2"
		out = append(out, Encode(control, nil)...)
	}
	if out != nil {
		out = append(out, "\x1b8"...) // DECRC, put the cursor back
	}
	return out
}
