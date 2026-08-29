package client

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

func testScreen(paint bool) (*screen, *bytes.Buffer) {
	var got bytes.Buffer
	return newScreen(&got, paint, slog.New(discardLogHandler{})), &got
}

// TestScreenHoldsInjectionUntilSequenceCompletes is the regression test for the reported bug.
//
// The title was written the instant the metadata arrived, which put it between the two halves of a
// truecolor SGR the session had split across a pty read. The terminal aborted the CSI, ate the OSC as a
// title, and printed the remainder, ":102:113m", as text on screen. That shifted the line, scrolled the
// screen, and left every cell nvim did not repaint stale until a ctrl-l.
//
// The whole byte stream is asserted rather than "the title appears somewhere", because the defect is
// entirely about where in the stream it appears.
func TestScreenHoldsInjectionUntilSequenceCompletes(t *testing.T) {
	scr, got := testScreen(true)
	const title = "\x1b]2;nvim\x07"

	// The bytes as the pty split them, from the capture that diagnosed this.
	if err := scr.session([]byte(" 30 \x1b(B\x1b[m\x1b[38:2:232")); err != nil {
		t.Fatalf("session() error = %v", err)
	}
	if err := scr.inject([]byte(title)); err != nil {
		t.Fatalf("inject() error = %v", err)
	}
	// Nothing may reach the terminal between the halves.
	if want := " 30 \x1b(B\x1b[m\x1b[38:2:232"; got.String() != want {
		t.Fatalf("after inject() the terminal has %q, want %q: the injection split the sequence",
			got.String(), want)
	}
	if err := scr.session([]byte(":102:113m-export")); err != nil {
		t.Fatalf("session() error = %v", err)
	}

	want := " 30 \x1b(B\x1b[m\x1b[38:2:232:102:113m-export" + title
	if got.String() != want {
		t.Errorf("terminal received\n  %q\nwant\n  %q", got.String(), want)
	}
}

func TestScreenInject(t *testing.T) {
	const title = "\x1b]2;t\x07"
	tests := []struct {
		name string
		// steps are applied in order. A step is either session bytes or an injection.
		steps []struct {
			inject bool
			data   string
		}
		paint bool
		want  string
	}{
		{
			name:  "at a boundary it goes straight out",
			paint: true,
			steps: []struct {
				inject bool
				data   string
			}{{false, "hello"}, {true, title}, {false, "world"}},
			want: "hello" + title + "world",
		},
		{
			name:  "held through an unterminated OSC",
			paint: true,
			steps: []struct {
				inject bool
				data   string
			}{{false, "\x1b]7;file://host/tmp"}, {true, title}, {false, "\x07done"}},
			want: "\x1b]7;file://host/tmp\x07done" + title,
		},
		{
			name:  "held through an unterminated DCS",
			paint: true,
			steps: []struct {
				inject bool
				data   string
			}{{false, "\x1bP+q4D73"}, {true, title}, {false, "\x1b\\"}},
			want: "\x1bP+q4D73\x1b\\" + title,
		},
		{
			name:  "two injections while held keep their order",
			paint: true,
			steps: []struct {
				inject bool
				data   string
			}{{false, "\x1b[38"}, {true, "\x1b]2;a\x07"}, {true, "\x1b]2;b\x07"}, {false, "m"}},
			want: "\x1b[38m\x1b]2;a\x07\x1b]2;b\x07",
		},
		{
			name:  "not painting a terminal, so nothing injected reaches the stream",
			paint: false,
			steps: []struct {
				inject bool
				data   string
			}{{false, "hello"}, {true, title}, {false, "world"}},
			want: "helloworld",
		},
		{
			name:  "an injection that is itself incomplete still holds the next one",
			paint: true,
			steps: []struct {
				inject bool
				data   string
			}{{true, "\x1b[38"}, {true, title}, {false, "m"}},
			want: "\x1b[38m" + title,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			scr, got := testScreen(tc.paint)
			for i, step := range tc.steps {
				var err error
				if step.inject {
					err = scr.inject([]byte(step.data))
				} else {
					err = scr.session([]byte(step.data))
				}
				if err != nil {
					t.Fatalf("step %d error = %v", i, err)
				}
			}
			if got.String() != tc.want {
				t.Errorf("terminal received\n  %q\nwant\n  %q", got.String(), tc.want)
			}
		})
	}
}

// TestScreenDropsRatherThanSplit covers the bound. An injection cannot be held forever, and the way out
// is to drop it: writing it anyway is the bug, and every byte cm injects is replaceable.
func TestScreenDropsRatherThanSplit(t *testing.T) {
	scr, got := testScreen(true)
	if err := scr.session([]byte("\x1b[38")); err != nil {
		t.Fatalf("session() error = %v", err)
	}
	// More than maxHeld of injections while the stream is mid-sequence.
	big := strings.Repeat("\x1b]2;x\x07", maxHeld)
	if err := scr.inject([]byte(big)); err != nil {
		t.Fatalf("inject() error = %v", err)
	}
	if err := scr.session([]byte("m")); err != nil {
		t.Fatalf("session() error = %v", err)
	}
	if want := "\x1b[38m"; got.String() != want {
		t.Errorf("terminal received %q, want %q: an oversized injection must be dropped, not written",
			got.String(), want)
	}
}

// TestScreenSessionBytesAreNeverWithheld states the other half of the contract. Whatever state the
// stream is in, the program's own bytes go out: holding them would trade a rendering fault for a stall.
func TestScreenSessionBytesAreNeverWithheld(t *testing.T) {
	scr, got := testScreen(true)
	for _, chunk := range []string{"\x1b[38", ":2:232", ":102:113m", "text"} {
		if err := scr.session([]byte(chunk)); err != nil {
			t.Fatalf("session() error = %v", err)
		}
	}
	if want := "\x1b[38:2:232:102:113mtext"; got.String() != want {
		t.Errorf("terminal received %q, want %q", got.String(), want)
	}
}
