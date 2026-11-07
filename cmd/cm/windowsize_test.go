package main

import (
	"os"
	"testing"

	"github.com/creack/pty"
)

// The first terminal among the files wins, skipping ones redirected elsewhere, as stdout is under
// `name=$(cm attach --no-attach)`.
func TestCallerSizeSkipsFilesThatAreNotTerminals(t *testing.T) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	defer ptmx.Close()
	defer tty.Close()
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 40, Cols: 150, X: 1200, Y: 800}); err != nil {
		t.Fatalf("pty.Setsize() error = %v", err)
	}

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe() error = %v", err)
	}
	defer r.Close()
	defer w.Close()

	type result struct {
		size windowSize
		ok   bool
	}
	size, ok := callerSize(w, tty)
	got := result{size: size, ok: ok}
	want := result{size: windowSize{rows: 40, cols: 150, xpixel: 1200, ypixel: 800}, ok: true}
	if got != want {
		t.Errorf("callerSize(pipe, tty) = %+v, want %+v", got, want)
	}
}

// With no terminal at all the caller falls back to the convention, so this has to say so.
func TestCallerSizeWithoutATerminal(t *testing.T) {
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("opening %s: %v", os.DevNull, err)
	}
	defer devNull.Close()

	type result struct {
		size windowSize
		ok   bool
	}
	size, ok := callerSize(devNull)
	got := result{size: size, ok: ok}
	want := result{}
	if got != want {
		t.Errorf("callerSize(/dev/null) = %+v, want %+v", got, want)
	}
}
