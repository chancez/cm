package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// windowSize is what a session is created at by a command that is not attaching to it.
type windowSize struct {
	rows   uint16
	cols   uint16
	xpixel uint16
	ypixel uint16
}

// conventionalSize is the size used when there is no terminal to ask. A program that checks gets a
// plausible answer rather than zeros. Pixels stay zero: they depend on a font, and zero is what a program
// reads as "this terminal cannot report pixel sizes".
var conventionalSize = windowSize{rows: 24, cols: 80}

// callerSize reports the size of the first of files that is a terminal.
//
// A session created at 80 columns and attached from a wider window keeps whatever its shell drew first at
// the old width: zsh's right prompt sat mid-line until the next prompt. The caller's own terminal is the
// best guess at the window that will attach, and usually is that window.
//
// Several files rather than one, because each is commonly redirected: `name=$(cm attach --no-attach)`
// captures stdout and leaves stderr on the terminal.
func callerSize(files ...*os.File) (windowSize, bool) {
	for _, f := range files {
		ws, err := unix.IoctlGetWinsize(int(f.Fd()), unix.TIOCGWINSZ)
		if err != nil || ws.Row == 0 || ws.Col == 0 {
			continue
		}
		return windowSize{rows: ws.Row, cols: ws.Col, xpixel: ws.Xpixel, ypixel: ws.Ypixel}, true
	}
	return windowSize{}, false
}

// parseSize reads --size, written COLSxROWS like a terminal's geometry.
//
// For a caller with no terminal of its own that knows the window a session will be shown in: an editor
// plugin creating a session for a split it is about to open. Pixels are left at zero, since a size given
// by hand says nothing about the font.
func parseSize(s string) (windowSize, error) {
	colsArg, rowsArg, ok := strings.Cut(s, "x")
	if !ok {
		return windowSize{}, fmt.Errorf("invalid size %q: want COLSxROWS, e.g. 120x40", s)
	}
	cols, colsErr := strconv.ParseUint(colsArg, 10, 16)
	rows, rowsErr := strconv.ParseUint(rowsArg, 10, 16)
	if colsErr != nil || rowsErr != nil || cols == 0 || rows == 0 {
		return windowSize{}, fmt.Errorf("invalid size %q: want COLSxROWS, each between 1 and 65535", s)
	}
	return windowSize{rows: uint16(rows), cols: uint16(cols)}, nil
}
