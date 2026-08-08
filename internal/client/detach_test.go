package client

import (
	"bytes"
	"testing"
)

func TestFindDetach(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  int
	}{
		{"plain text", []byte("ls -la\r"), -1},
		{"empty", nil, -1},

		{"raw ctrl-backslash", []byte{DetachKey}, 0},
		{"raw after input", []byte("ls" + string(rune(DetachKey))), 2},

		// A terminal with the kitty keyboard protocol or modifyOtherKeys enabled reports
		// the key as a CSI sequence, so only checking for 0x1C would silently stop working
		// for exactly the users most likely to have those modes on.
		{"kitty keyboard encoding", []byte("\x1b[92;5u"), 0},
		{"kitty after input", []byte("ab\x1b[92;5u"), 2},
		{"modifyOtherKeys encoding", []byte("\x1b[27;5;92~"), 0},

		// Other keys that share a prefix must not be mistaken for detach.
		{"unmodified backslash", []byte("\\"), -1},
		{"ctrl-c", []byte{0x03}, -1},
		{"unrelated CSI", []byte("\x1b[A"), -1},
		{"different kitty key", []byte("\x1b[97;5u"), -1},

		// When several appear, the earliest wins: everything after a detach is discarded.
		{"earliest of two", []byte("a\x1b[92;5ub" + string(rune(DetachKey))), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FindDetach(tt.input); got != tt.want {
				t.Errorf("FindDetach(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMightStartDetach(t *testing.T) {
	tests := []struct {
		name  string
		input []byte
		want  bool
	}{
		// Terminal input arrives in arbitrary pieces, so a CSI-encoded detach can straddle
		// two reads. Without holding back a possible prefix, both halves reach the shell
		// and the detach is missed.
		{"partial kitty prefix", []byte("\x1b"), true},
		{"longer partial", []byte("\x1b[92;"), true},
		{"partial after text", []byte("ls \x1b[92"), true},
		{"partial modifyOtherKeys", []byte("\x1b[27;5;"), true},

		{"complete sequence is not partial", []byte("\x1b[92;5u"), false},
		{"unrelated text", []byte("hello"), false},
		{"empty", nil, false},
		// An escape that has already diverged cannot become a detach.
		{"diverged CSI", []byte("\x1b[A"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mightStartDetach(tt.input); got != tt.want {
				t.Errorf("mightStartDetach(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// The property that matters: a detach split across reads is still detected once reassembled,
// and the bytes before it are preserved for the shell.
func TestDetachSplitAcrossReads(t *testing.T) {
	full := []byte("echo hi\x1b[92;5u")

	for split := 1; split < len(full); split++ {
		first, second := full[:split], full[split:]

		// Simulate the client's buffering: hold back a possible partial sequence.
		var held []byte
		buf := append(held, first...)
		if FindDetach(buf) >= 0 {
			// Detach was entirely in the first read; nothing more to check.
			continue
		}
		if mightStartDetach(buf) {
			keep := len(buf)
			for _, seq := range detachSequences {
				if n := len(seq) - 1; n < keep {
					keep = n
				}
			}
			if keep > 0 && keep <= len(buf) {
				held = append(held, buf[len(buf)-keep:]...)
				buf = buf[:len(buf)-keep]
			}
		}
		forwarded := append([]byte(nil), buf...)

		buf = append(held, second...)
		i := FindDetach(buf)
		if i < 0 {
			t.Errorf("split at %d: detach not found after reassembly (forwarded %q, then %q)",
				split, forwarded, buf)
			continue
		}
		forwarded = append(forwarded, buf[:i]...)

		if !bytes.Equal(forwarded, []byte("echo hi")) {
			t.Errorf("split at %d: forwarded %q, want %q", split, forwarded, "echo hi")
		}
	}
}
