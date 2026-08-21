package server

import (
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Everything a client says about a session it is creating has to reach the session.
//
// This is the seam a real bug lived at. Open carried x_pixel and y_pixel, Resize plumbed them through
// correctly, and this translation dropped them, so a session's pty was created with zero pixel
// dimensions and only got real ones if something later resized it. `kitten icat` reads exactly that
// field before it transmits anything, treats zero as "this terminal cannot report pixel sizes", and
// refuses to send the image while naming kitty as a terminal to use instead. Nothing pointed at cm.
//
// The whole struct is compared rather than the pixel fields alone, which is the point: a field-by-field
// check is what passes while the field nobody thought to assert is silently dropped.
func TestOpenOptionsFromCopiesEveryField(t *testing.T) {
	open := &serverv1.Open{
		Session:       "sess",
		Rows:          30,
		Cols:          100,
		XPixel:        800,
		YPixel:        600,
		Command:       []string{"/bin/sh", "-c", "true"},
		Cwd:           "/tmp",
		Env:           []string{"FOO=bar"},
		ClientEnv:     map[string]string{"TERM": "xterm-kitty"},
		Persist:       true,
		CaptureOutput: true,
		OnRestore:     "shell",
		Tags:          map[string]string{"role": "build"},
	}

	got := openOptionsFrom(open)
	want := OpenOptions{
		Name:          "sess",
		Rows:          30,
		Cols:          100,
		XPixel:        800,
		YPixel:        600,
		Command:       []string{"/bin/sh", "-c", "true"},
		Dir:           "/tmp",
		Env:           []string{"FOO=bar"},
		ClientEnv:     map[string]string{"TERM": "xterm-kitty"},
		Persist:       true,
		CaptureOutput: true,
		OnRestore:     RestoreShell,
		Tags:          map[string]string{"role": "build"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("openOptionsFrom() = %+v, want %+v", got, want)
	}
}

// A client that reported no pixel size must have none invented for it: zero is what a program reads as
// "the terminal does not know", so a made-up value would have it compute cell sizes from a window that
// does not exist. `cm attach --no-attach` is the real case, having no terminal to ask.
func TestOpenOptionsFromLeavesUnknownPixelSizeZero(t *testing.T) {
	got := openOptionsFrom(&serverv1.Open{Session: "sess", Rows: 24, Cols: 80})
	want := OpenOptions{Name: "sess", Rows: 24, Cols: 80}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("openOptionsFrom() = %+v, want %+v", got, want)
	}
}
