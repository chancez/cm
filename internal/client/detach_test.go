package client

import (
	"bytes"
	"testing"
)

func TestFindDetach(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	tests := []struct {
		name  string
		input []byte
		want  int
	}{
		{"plain text", []byte("ls -la\r"), -1},
		{"empty", nil, -1},

		{"raw ctrl-backslash", []byte{0x1C}, 0},
		{"raw after input", []byte("ls\x1c"), 2},

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
		{"earliest of two", []byte("a\x1b[92;5ub\x1c"), 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := key.Find(tt.input); got != tt.want {
				t.Errorf("FindDetach(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestMightStartDetach(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

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
			if got := key.MightStart(tt.input); got != tt.want {
				t.Errorf("mightStartDetach(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// The property that matters: a detach split across reads is still detected once reassembled,
// and the bytes before it are preserved for the shell.
func TestDetachSplitAcrossReads(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	full := []byte("echo hi\x1b[92;5u")

	for split := 1; split < len(full); split++ {
		first, second := full[:split], full[split:]

		// Simulate the client's buffering: hold back a possible partial sequence.
		var held []byte
		buf := append(held, first...)
		if key.Find(buf) >= 0 {
			// Detach was entirely in the first read; nothing more to check.
			continue
		}
		if keep := key.HoldBack(buf); keep > 0 && keep <= len(buf) {
			held = append(held, buf[len(buf)-keep:]...)
			buf = buf[:len(buf)-keep]
		}
		forwarded := append([]byte(nil), buf...)

		buf = append(held, second...)
		i := key.Find(buf)
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

// A terminal reply must reach the shell whole, in the read it arrived in.
//
// HoldBack computed its length from the *shortest* configured sequence rather than from how much of
// a sequence the tail actually matches, so any chunk whose last byte could begin a detach lost six
// trailing bytes to the hold buffer. Those bytes were not dropped, they were delayed until the next
// keystroke, which is worse: a program that queries the terminal and waits for an answer gets a
// truncated reply, and the missing tail is later pasted into the shell's line editor.
//
// Symptom that produced this: `wallfacer -h` left ";rgb:2828/2c2c/3434\x1b\\\x1b[3;1R" on screen and
// "execute: 2828/2c2c/3434" in the prompt, because the OSC 11 background-color reply arrived in a
// chunk ending with the ESC of its ST terminator.
func TestHoldBackKeepsOnlyAPossiblePrefix(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	tests := []struct {
		name  string
		input []byte
		want  int
	}{
		// An OSC 11 reply whose chunk ends at the ESC of its ST terminator. Only that final ESC
		// could begin a detach, so only it may be held.
		{"osc reply ending in ESC", []byte("\x1b]11;rgb:2828/2c2c/3434\x1b"), 1},
		{"text then trailing ESC", []byte("abcdefgh\x1b"), 1},
		{"bare ESC", []byte("\x1b"), 1},
		{"ESC bracket", []byte("\x1b["), 2},
		{"partial modifyOtherKeys", []byte("\x1b[27;5;"), 7},

		// Nothing to hold when the tail cannot begin a detach.
		{"complete detach", []byte("\x1b[92;5u"), 0},
		{"plain text", []byte("ls -la"), 0},
		{"diverged CSI", []byte("\x1b[A"), 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := key.HoldBack(tt.input); got != tt.want {
				t.Errorf("HoldBack(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

// The bytes a client forwards must be exactly the bytes it read, minus at most a real partial
// detach. This is the property the bug violated: it forwarded a prefix and stranded the rest.
func TestHoldBackDoesNotStrandTerminalReplyBytes(t *testing.T) {
	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	// An OSC 11 background-color reply followed by a cursor position report, as termenv-style
	// probing produces, split at the point that strands bytes.
	reply := []byte("\x1b]11;rgb:2828/2c2c/3434\x1b")

	var held []byte
	buf := append(held, reply...)
	if i := key.Find(buf); i >= 0 {
		t.Fatalf("Find(%q) = %d, want no detach in a terminal reply", buf, i)
	}
	if keep := key.HoldBack(buf); keep > 0 && keep <= len(buf) {
		held = append(held, buf[len(buf)-keep:]...)
		buf = buf[:len(buf)-keep]
	}

	// Everything except a possible partial detach must have gone through. The only byte here that
	// could begin one is the trailing ESC.
	wantForwarded := reply[:len(reply)-1]
	if !bytes.Equal(buf, wantForwarded) {
		t.Errorf("forwarded %q, want %q", buf, wantForwarded)
	}
	if !bytes.Equal(held, []byte("\x1b")) {
		t.Errorf("held %q, want %q", held, "\x1b")
	}
}
