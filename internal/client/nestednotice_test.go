package client

import (
	"bytes"
	"strings"
	"testing"
)

// The notice names the key, because the escape is otherwise folklore: nothing else on screen says that a
// third press leaves this session rather than the one nested inside it.
func TestNestedNoticePaintsOnTheBottomRow(t *testing.T) {
	var buf bytes.Buffer
	n := &nestedNotice{out: &buf, size: fixedSize(24, 80), enabled: true}

	key, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey(%q) error = %v", DefaultDetachKey, err)
	}
	n.show(key)

	got := buf.String()
	if !strings.HasPrefix(got, "\x1b7\x1b[24;1H\x1b[2K\x1b[7m") || !strings.HasSuffix(got, "\x1b[0m\x1b8") {
		t.Errorf("painted %q, want the cursor saved, the last row cleared, and the cursor restored", got)
	}
	if !strings.Contains(got, key.Name) {
		t.Errorf("painted %q, want it to name %q so the escape is discoverable", got, key.Name)
	}
	if !n.painted {
		t.Error("painted = false after showing, so nothing would repaint the row it covered")
	}
}

// Painted once rather than on every press, since a terminal being written to is a terminal that cannot be
// idle, and the row already says what it needs to.
func TestNestedNoticePaintsOnce(t *testing.T) {
	var buf bytes.Buffer
	n := &nestedNotice{out: &buf, size: fixedSize(24, 80), enabled: true}
	key, _ := ParseDetachKey(DefaultDetachKey)

	n.show(key)
	first := buf.Len()
	n.show(key)

	if buf.Len() != first {
		t.Errorf("a second show wrote %d more bytes, want none", buf.Len()-first)
	}
}

// clear reports whether a row was covered, which is what tells the caller it owes a repaint from cm's model.
func TestNestedNoticeClearReportsWhetherItHadPainted(t *testing.T) {
	var buf bytes.Buffer
	n := &nestedNotice{out: &buf, size: fixedSize(24, 80), enabled: true}
	key, _ := ParseDetachKey(DefaultDetachKey)

	if n.clear() {
		t.Error("clear() = true with nothing painted, which would cost a repaint for nothing")
	}
	n.show(key)
	buf.Reset()
	if !n.clear() {
		t.Error("clear() = false after painting, so the row it covered would stay covered")
	}
	if got := buf.String(); got != "\x1b7\x1b[24;1H\x1b[2K\x1b8" {
		t.Errorf("clear wrote %q, want the row erased with the cursor put back", got)
	}
	if n.clear() {
		t.Error("clear() = true twice, want false the second time")
	}
}

func TestNestedNoticeStaysQuiet(t *testing.T) {
	key, _ := ParseDetachKey(DefaultDetachKey)
	tests := []struct {
		name   string
		notice *nestedNotice
	}{
		{
			name: "not painting a terminal",
			// A follower streaming to a pipe: an escape sequence there is corruption in someone's file.
			notice: &nestedNotice{size: fixedSize(24, 80), enabled: false},
		},
		{
			name: "size unknown",
			// A row number guessed wrong would write into the middle of the session.
			notice: &nestedNotice{size: fixedSize(0, 0), enabled: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			tt.notice.out = &buf
			tt.notice.show(key)
			if buf.Len() != 0 {
				t.Errorf("wrote %q, want nothing", buf.String())
			}
			if tt.notice.painted {
				t.Error("painted = true, but nothing was written")
			}
		})
	}
}
