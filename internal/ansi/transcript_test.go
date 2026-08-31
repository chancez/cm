package ansi

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		writes []Write
		// want is a substring of the expected complaint, or empty for a clean transcript.
		want string
	}{
		{
			name: "nothing at all",
		},
		{
			name:   "plain output",
			writes: []Write{{Data: []byte("hello")}},
		},
		{
			name: "an injection between two complete sequences",
			writes: []Write{
				{Data: []byte("\x1b[0mhello")},
				{Data: []byte("\x1b]2;title\x07"), Injected: true},
				{Data: []byte("world\x1b[0m")},
			},
		},
		{
			// The reported bug, as a transcript. The program's SGR is split across two writes because a pty
			// read ended inside it, and the title goes out in the gap.
			name: "an injection inside the program's sequence",
			writes: []Write{
				{Data: []byte(" 30 \x1b(B\x1b[m\x1b[38:2:232")},
				{Data: []byte("\x1b]2;nvim\x07"), Injected: true},
				{Data: []byte(":102:113m-export")},
			},
			want: "while the program is mid-sequence",
		},
		{
			name: "a split sequence with nothing in the gap is fine",
			writes: []Write{
				{Data: []byte("\x1b[38:2:232")},
				{Data: []byte(":102:113m")},
			},
		},
		{
			name: "an injection inside a split OSC",
			writes: []Write{
				{Data: []byte("\x1b]7;file://host/tmp")},
				{Data: []byte("\x1b]2;t\x07"), Injected: true},
				{Data: []byte("\x07")},
			},
			want: "while the program is mid-sequence",
		},
		{
			name: "an injection inside a split DCS",
			writes: []Write{
				{Data: []byte("\x1bP+q4D73")},
				{Data: []byte("\x1b]2;t\x07"), Injected: true},
				{Data: []byte("\x1b\\")},
			},
			want: "while the program is mid-sequence",
		},
		{
			name: "an injection that is itself incomplete",
			writes: []Write{
				{Data: []byte("hello")},
				{Data: []byte("\x1b]2;unterminated"), Injected: true},
			},
			want: "which is an incomplete sequence",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			problems := Validate(tc.writes)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("Validate() = %v, want no problems", problems)
				}
				return
			}
			if len(problems) == 0 {
				t.Fatalf("Validate() found nothing, want a complaint containing %q", tc.want)
			}
			found := false
			for _, p := range problems {
				if strings.Contains(p.Error(), tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("Validate() = %v, want one containing %q", problems, tc.want)
			}
		})
	}
}

func TestSessionBytes(t *testing.T) {
	writes := []Write{
		{Data: []byte("one")},
		{Data: []byte("\x1b]2;t\x07"), Injected: true},
		{Data: []byte("two")},
		{Data: []byte("\x1b]11;?\x07"), Injected: true},
		{Data: []byte("three")},
	}
	if got, want := string(SessionBytes(writes)), "onetwothree"; got != want {
		t.Errorf("SessionBytes() = %q, want %q", got, want)
	}
}

// The other reading of the same transcript: what the terminal actually received.
//
// A test asking whether cm delivered something it generated has to use this one. SessionBytes drops exactly
// those bytes, which reported a missing image for a stream that carried it.
func TestAllBytes(t *testing.T) {
	writes := []Write{
		{Data: []byte("one")},
		{Data: []byte("\x1b]2;t\x07"), Injected: true},
		{Data: []byte("two")},
	}
	if got, want := string(AllBytes(writes)), "one\x1b]2;t\x07two"; got != want {
		t.Errorf("AllBytes() = %q, want %q", got, want)
	}
}
