package vt

import (
	"slices"
	"testing"
)

// A session's model answers the OSC 7501 support query.
//
// cm reads program status reports itself, so it supports the protocol whatever terminal is attached, and
// with none attached. Unanswered, a program asking concludes the terminal does not support it and never
// reports: libghostty stays silent on the query unless a program status callback is installed.
//
// Through NewSessionTerminal rather than New, since that is the path a session's reply takes, DenyModes
// included.
func TestSessionTerminalAnswersProgramStatusQuery(t *testing.T) {
	term, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer term.Close()

	if err := term.Write([]byte("\x1b]7501;?\x1b\\")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	want := [][]byte{[]byte("\x1b]7501;?:states=idle,working,done,blocked,error:kinds=permission,question,auth\x1b\\")}
	if got := term.TakePending(); !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("TakePending() = %q, want %q", got, want)
	}
}
