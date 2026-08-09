package seqlog

import (
	"bytes"
	"strings"
	"testing"
)

func TestSnapshotFromAPosition(t *testing.T) {
	l := New(1024)
	l.Append([]byte("hello "))
	l.Append([]byte("world"))

	tests := []struct {
		name string
		from uint64
		want string
	}{
		{name: "from the start", from: 0, want: "hello world"},
		{name: "mid-chunk", from: 6, want: "world"},
		{name: "inside a chunk", from: 3, want: "lo world"},
		// The very end is empty rather than an error: a command that just started has printed nothing.
		{name: "at the end", from: 11, want: ""},
		// Past the end is the same, which happens when a boundary is recorded and the output has not
		// arrived yet.
		{name: "past the end", from: 99, want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, gap := l.Snapshot(tc.from)
			if gap {
				t.Errorf("Snapshot(%d) reported a gap, want none", tc.from)
			}
			if string(got) != tc.want {
				t.Errorf("Snapshot(%d) = %q, want %q", tc.from, got, tc.want)
			}
		})
	}
}

// A position already trimmed away is served from the oldest byte and flagged.
//
// The flag is what tells a caller its view is not what it asked for. For a read anchored at a command
// boundary it means the command's own output has partly aged out, which is different from the command
// having printed little.
func TestSnapshotReportsAGap(t *testing.T) {
	l := New(10)
	l.Append([]byte("0123456789"))
	// Pushes the first five bytes out.
	l.Append([]byte("abcde"))

	oldest, next := l.Bounds()
	if oldest != 5 || next != 15 {
		t.Fatalf("Bounds() = (%d, %d), want (5, 15)", oldest, next)
	}

	// Asking from before what is retained.
	got, gap := l.Snapshot(0)
	if !gap {
		t.Error("Snapshot(0) reported no gap, want one: those bytes were dropped")
	}
	if string(got) != "56789abcde" {
		t.Errorf("Snapshot(0) = %q, want the oldest retained bytes", got)
	}

	// Asking from exactly the oldest is not a gap: nothing it asked for is missing.
	got, gap = l.Snapshot(5)
	if gap {
		t.Error("Snapshot(oldest) reported a gap, want none")
	}
	if string(got) != "56789abcde" {
		t.Errorf("Snapshot(5) = %q, want everything retained", got)
	}
}

// The returned bytes must not alias the buffer, which is trimmed from the front as output arrives.
//
// A retained slice would shift out from under the caller: the same indices would name different bytes
// after a trim, so a snapshot taken before one and read after it would return output from the wrong
// position.
func TestSnapshotDoesNotAliasTheBuffer(t *testing.T) {
	l := New(16)
	l.Append([]byte("AAAAAAAA"))

	got, _ := l.Snapshot(0)
	if string(got) != "AAAAAAAA" {
		t.Fatalf("Snapshot(0) = %q, want %q", got, "AAAAAAAA")
	}

	// Enough output to trim what was just read.
	l.Append([]byte(strings.Repeat("B", 16)))

	if string(got) != "AAAAAAAA" {
		t.Errorf("the snapshot changed to %q after an append, want it to be a copy", got)
	}
}

// An empty log answers empty rather than failing.
func TestSnapshotOnAnEmptyLog(t *testing.T) {
	l := New(64)
	got, gap := l.Snapshot(0)
	if len(got) != 0 || gap {
		t.Errorf("Snapshot(0) = (%q, gap=%v) on an empty log, want (empty, false)", got, gap)
	}
}

// A log numbering from an offset answers in those numbers, not from zero.
//
// The server's log continues the shim's numbering, so a position named by a boundary is in that space.
// Treating it as an offset from zero would read from the wrong place by however far in the session was
// when the server started following it.
func TestSnapshotHonorsAStartingOffset(t *testing.T) {
	const start = 1000
	l := NewAt(64, start)
	l.Append([]byte("offset output"))

	got, gap := l.Snapshot(start + 7)
	if gap {
		t.Error("Snapshot() reported a gap, want none")
	}
	if string(got) != "output" {
		t.Errorf("Snapshot(%d) = %q, want %q", start+7, got, "output")
	}

	// And a position below the origin is a gap rather than an underflow.
	got, gap = l.Snapshot(0)
	if !gap {
		t.Error("Snapshot(0) on a log starting at 1000 reported no gap, want one")
	}
	if !bytes.Equal(got, []byte("offset output")) {
		t.Errorf("Snapshot(0) = %q, want everything retained", got)
	}
}
