package vt

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestContinuation pins what a fresh terminal is handed to pick up mid-sequence, for the shapes that a
// hand-written tracker got wrong: a string control past its 4096 byte give-up point, a split UTF-8
// character, and ESC restarting a CSI. Each was reported as "between sequences" by ansi.Tracker, so an
// attaching client received the tail of the sequence as text.
func TestContinuation(t *testing.T) {
	tests := []struct {
		name  string
		write string
		want  []byte
	}{
		{name: "between sequences", write: "\x1b]2;title\x07text", want: nil},
		{name: "csi", write: "text\x1b[38:2:1", want: []byte("\x1b[38:2:1")},
		{name: "long osc", write: "\x1b]52;c;" + strings.Repeat("A", 5000), want: []byte("\x1b]52;c;" + strings.Repeat("A", 5000))},
		{name: "long dcs", write: "\x1bPq" + strings.Repeat("~", 5000), want: []byte("\x1bPq" + strings.Repeat("~", 5000))},
		{name: "split utf-8", write: "caf\xc3", want: []byte("\xc3")},
		{name: "esc inside csi", write: "\x1b[3\x1b[1", want: []byte("\x1b[1")},
		// The case that rules out rewinding the byte stream by the continuation's length: the line feed has
		// already moved the cursor, so it is not part of what is pending.
		{name: "control inside csi", write: "\x1b[3\n1", want: []byte("\x1b[31")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			term, err := New(24, 80, Callbacks{})
			if err != nil {
				t.Fatal(err)
			}
			defer term.Close()
			if err := term.Write([]byte(tt.write)); err != nil {
				t.Fatal(err)
			}
			got, err := term.Continuation()
			if err != nil {
				t.Fatalf("Continuation() error = %v", err)
			}
			if !bytes.Equal(got, tt.want) {
				t.Errorf("Continuation() = %q, want %q", trunc(got), trunc(tt.want))
			}
		})
	}
}

// TestContinuationPastTheLimit is what a caller has to handle: a sequence longer than MaxContinuation is
// not tracked, and saying so is better than handing back a prefix that would complete wrongly.
func TestContinuationPastTheLimit(t *testing.T) {
	term, err := New(24, 80, Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Write([]byte("\x1b]52;c;" + strings.Repeat("A", MaxContinuation))); err != nil {
		t.Fatal(err)
	}
	got, err := term.Continuation()
	if !errors.Is(err, ErrInvalid) || got != nil {
		t.Errorf("Continuation() = %q, %v, want nil, %v", trunc(got), err, ErrInvalid)
	}
}

func trunc(b []byte) string {
	if len(b) <= 60 {
		return string(b)
	}
	return string(b[:30]) + "..." + string(b[len(b)-30:])
}
