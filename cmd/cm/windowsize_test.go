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

func TestParseSize(t *testing.T) {
	type result struct {
		size windowSize
		err  bool
	}
	tests := []struct {
		in   string
		want result
	}{
		{in: "120x40", want: result{size: windowSize{rows: 40, cols: 120}}},
		{in: "65535x1", want: result{size: windowSize{rows: 1, cols: 65535}}},
		{in: "120", want: result{err: true}},
		{in: "0x40", want: result{err: true}},
		{in: "120x0", want: result{err: true}},
		{in: "65536x40", want: result{err: true}},
		{in: "-1x40", want: result{err: true}},
		{in: "120X40", want: result{err: true}},
		{in: "", want: result{err: true}},
	}
	for _, tt := range tests {
		size, err := parseSize(tt.in)
		got := result{size: size, err: err != nil}
		if got != tt.want {
			t.Errorf("parseSize(%q) = %+v (err %v), want %+v", tt.in, got, err, tt.want)
		}
	}
}
