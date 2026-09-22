package query

import "fmt"

// Report answers an XTWINOPS size report from what cm knows about the session, or reports that it cannot.
//
// These three reports were proxied to a client until measurement showed what that costs. A proxied query
// with nobody to answer it parks and expires after requestTimeout, and every reply queued behind it waits
// that out: measured at 507ms, 510ms and 525ms on a session with no client attached, where nothing could
// ever have answered. The asking program gets nothing at all, which docs/testing.md calls the worse of the
// two failures, since the symptom is a hang rather than an artifact.
//
// cm holds the answers. It writes the pty's winsize itself and every resize passes through it, so the cell
// dimensions are not a guess, and the pixel dimensions are what an attaching client reported. See
// docs/architecture.md on delivering an answer to a program that has gone.
//
// Which of the three cm should answer *instead of* asking is not the same question, and is decided by the
// caller rather than here. 18t is cm's outright: no terminal's answer is better, because the value is the
// one cm set. 14t and 16t are better answered by a real terminal when one is attached, because a font
// change moves the cell size and only the terminal knows, so those fall back to this only when no client
// can answer.
//
// Reports false when the metrics needed are zero, which is a pty that was never given pixel dimensions.
// Answering 0 there would be worse than silence: zero is what `kitten icat` reads as "this terminal cannot
// report pixel sizes", and it then refuses to transmit an image at all rather than falling back.
func Report(p []byte, rows, cols, xpixel, ypixel uint16) ([]byte, bool) {
	op, ok := winopsReport(p)
	if !ok {
		return nil, false
	}
	switch op {
	case 18:
		// Text area in characters. The one cm is authoritative about.
		if rows == 0 || cols == 0 {
			return nil, false
		}
		return []byte(fmt.Sprintf("\x1b[8;%d;%dt", rows, cols)), true
	case 14:
		// Text area in pixels.
		if xpixel == 0 || ypixel == 0 {
			return nil, false
		}
		return []byte(fmt.Sprintf("\x1b[4;%d;%dt", ypixel, xpixel)), true
	case 16:
		// Cell size in pixels, which a terminal derives the same way rather than storing separately.
		// Integer division, so a fractional cell size rounds down here as it does there.
		if xpixel == 0 || ypixel == 0 || rows == 0 || cols == 0 {
			return nil, false
		}
		return []byte(fmt.Sprintf("\x1b[6;%d;%dt", ypixel/rows, xpixel/cols)), true
	}
	return nil, false
}

// winopsReport reports which XTWINOPS report p asks for, if it is one.
//
// Shared by Report and the classifier so the two cannot disagree about what counts as a report: a sequence
// classified as answerable here and unrecognized there would be registered as outstanding and never
// answered, which stalls every reply behind it for the full timeout.
func winopsReport(p []byte) (int, bool) {
	params, inter, final, _, ok := parseCSI(p)
	if !ok || final != 't' || len(inter) > 0 {
		return 0, false
	}
	// A private marker means something else; the report forms are unmarked.
	if len(params) > 0 && !isDigit(params[0]) {
		return 0, false
	}
	i := 0
	for i < len(params) && isDigit(params[i]) {
		i++
	}
	switch string(params[:i]) {
	case "14":
		return 14, true
	case "16":
		return 16, true
	case "18":
		return 18, true
	}
	return 0, false
}
