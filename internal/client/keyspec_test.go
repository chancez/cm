package client

import "testing"

func TestParseDetachKey(t *testing.T) {
	tests := []struct {
		spec     string
		wantByte byte
		wantName string
		disabled bool
		wantErr  bool
	}{
		{spec: `ctrl-\`, wantByte: 0x1C, wantName: `ctrl-\`},
		{spec: "ctrl-a", wantByte: 0x01, wantName: "ctrl-a"},
		{spec: "ctrl-z", wantByte: 0x1A, wantName: "ctrl-z"},
		{spec: "ctrl-]", wantByte: 0x1D, wantName: "ctrl-]"},
		{spec: "ctrl-_", wantByte: 0x1F, wantName: "ctrl-_"},
		// A named key that is one character resolves through the same table `cm send --key` uses, so
		// the key with the best ergonomics on a keyboard is spellable at all. NUL is what a terminal
		// sends for it, when it sends anything.
		{spec: "ctrl-space", wantByte: 0x00, wantName: "ctrl-space"},
		// Short form and case insensitivity, since a config file is hand-written.
		{spec: "c-a", wantByte: 0x01, wantName: "ctrl-a"},
		{spec: "CTRL-A", wantByte: 0x01, wantName: "ctrl-a"},
		{spec: "  ctrl-a  ", wantByte: 0x01, wantName: "ctrl-a"},
		// Empty falls back to the default rather than disabling, since an unset config setting
		// should not silently remove the only way to detach.
		{spec: "", wantByte: 0x1C, wantName: `ctrl-\`},
		{spec: "none", disabled: true, wantName: "none"},
		{spec: "off", disabled: true, wantName: "none"},

		{spec: "a", wantErr: true},
		{spec: "ctrl-", wantErr: true},
		{spec: "ctrl-ab", wantErr: true},
		{spec: "alt-a", wantErr: true},
		// No control code exists for these, so accepting them would produce a key that can never
		// be pressed.
		{spec: "ctrl-1", wantErr: true},
		{spec: "ctrl-,", wantErr: true},
		// Named keys whose own byte is already a control code, so they are not ctrl- combinations:
		// ctrl-[ is how the escape key is spelled.
		{spec: "ctrl-esc", wantErr: true},
		{spec: "ctrl-enter", wantErr: true},
		{spec: "ctrl-up", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.spec, func(t *testing.T) {
			got, err := ParseDetachKey(tt.spec)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseDetachKey(%q) error = %v, wantErr %v", tt.spec, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if got.Disabled != tt.disabled {
				t.Errorf("Disabled = %v, want %v", got.Disabled, tt.disabled)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if !tt.disabled && got.Byte != tt.wantByte {
				t.Errorf("Byte = %#x, want %#x", got.Byte, tt.wantByte)
			}
		})
	}
}

// The two keys default independently, and to different keys: they are both live at once, so a default
// shared between them would leave the overlay unreachable or detaching unreachable depending on which
// won.
func TestParsePrefixKeyDefaults(t *testing.T) {
	prefix, err := ParsePrefixKey("")
	if err != nil {
		t.Fatalf("ParsePrefixKey(\"\") error = %v", err)
	}
	want := KeySpec{
		Byte:      0x1D,
		Sequences: encodingsFor(']'),
		Name:      "ctrl-]",
	}
	if prefix.Byte != want.Byte || prefix.Name != want.Name || prefix.Disabled {
		t.Errorf("ParsePrefixKey(\"\") = %+v, want %+v", prefix, want)
	}

	detach, err := ParseDetachKey("")
	if err != nil {
		t.Fatalf("ParseDetachKey(\"\") error = %v", err)
	}
	if detach.Byte == prefix.Byte {
		t.Errorf("the default prefix and detach keys are both %#x, so one of them is unreachable",
			detach.Byte)
	}
}

// A configured key must be detected in all the encodings a terminal may use, not just the control
// byte. zmx hit this with a program that enables modifyOtherKeys on startup, which made its detach
// key stop working entirely.
func TestParseDetachKeyCoversAllEncodings(t *testing.T) {
	key, err := ParseDetachKey("ctrl-q")
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}

	// 'q' is codepoint 113, and ctrl is modifier 5 in both protocols.
	for _, input := range []string{
		"\x11",           // raw control byte
		"\x1b[113;5u",    // kitty keyboard
		"\x1b[27;5;113~", // xterm modifyOtherKeys
	} {
		if got := key.Find([]byte(input)); got != 0 {
			t.Errorf("Find(%q) = %d, want 0", input, got)
		}
	}

	// And must not match a different key's encodings.
	for _, input := range []string{"\x12", "\x1b[114;5u", "\x1b[27;5;114~"} {
		if got := key.Find([]byte(input)); got != -1 {
			t.Errorf("Find(%q) = %d, want -1", input, got)
		}
	}
}

// find reports the length as well as the offset, and the length is what lets the prefix key hand the
// rest of the read to the overlay. Asserted per encoding because they differ in length, which is the
// whole reason a caller cannot assume one byte: `prefix` then `d` typed quickly arrives as one read, and
// a caller that skipped one byte would feed "[93;5ud" to the overlay and act on "[".
func TestKeySpecFindReportsTheLength(t *testing.T) {
	key, err := ParsePrefixKey("ctrl-]")
	if err != nil {
		t.Fatalf("ParsePrefixKey() error = %v", err)
	}
	tests := []struct {
		in         string
		wantOffset int
		wantLength int
	}{
		{in: "\x1d", wantOffset: 0, wantLength: 1},
		{in: "ab\x1dd", wantOffset: 2, wantLength: 1},
		{in: "\x1b[93;5ud", wantOffset: 0, wantLength: 7},
		{in: "\x1b[27;5;93~d", wantOffset: 0, wantLength: 10},
		{in: "abc", wantOffset: -1, wantLength: 0},
	}
	for _, tt := range tests {
		offset, length := key.find([]byte(tt.in))
		if offset != tt.wantOffset || length != tt.wantLength {
			t.Errorf("find(%q) = (%d, %d), want (%d, %d)",
				tt.in, offset, length, tt.wantOffset, tt.wantLength)
		}
	}
}

// Disabling must let the key through to the session, which is the entire point: a program inside
// may want it.
func TestDisabledDetachKeyMatchesNothing(t *testing.T) {
	key, err := ParseDetachKey("none")
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	for _, input := range []string{"\x1c", "\x1b[92;5u", "\x1b[27;5;92~"} {
		if got := key.Find([]byte(input)); got != -1 {
			t.Errorf("Find(%q) = %d with detaching disabled, want -1", input, got)
		}
	}
	if key.MightStart([]byte("\x1b[92")) {
		t.Error("MightStart returned true with detaching disabled, so input would be held back forever")
	}
}
