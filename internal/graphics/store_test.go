package graphics

import (
	"fmt"
	"strings"
	"testing"
)

// mustParse parses a command a test constructed, failing rather than returning a zero value.
func mustParse(t *testing.T, raw string) Command {
	t.Helper()
	cmd, _, ok := Parse([]byte(raw))
	if !ok {
		t.Fatalf("Parse(%q) ok = false", raw)
	}
	return cmd
}

// A stored image re-emits as the bytes the program sent, which is the point of keeping payloads rather
// than reading them back out of libghostty.
func TestStoreRetransmitsVerbatimPayload(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T,f=100,s=4,v=3,i=1;iVBORw0KGgo\x1b\\"))

	got := s.Retransmissions()
	if len(got) != 1 {
		t.Fatalf("Retransmissions() returned %d commands, want 1", len(got))
	}
	if got[0].ID != 1 || got[0].ByNumber {
		t.Errorf("identified image as (%d, byNumber=%v), want (1, false)", got[0].ID, got[0].ByNumber)
	}

	// The payload has to survive byte for byte, since re-encoding is what this design avoids.
	cmd := mustParse(t, string(got[0].Bytes))
	if string(cmd.Payload) != "iVBORw0KGgo" {
		t.Errorf("re-emitted payload = %q, want the transmitted %q", cmd.Payload, "iVBORw0KGgo")
	}
	// And the geometry, or the terminal would draw it at the wrong size.
	for _, want := range []string{"s=4", "v=3", "f=100"} {
		if !strings.Contains(cmd.Control, want) {
			t.Errorf("re-emitted control %q lost %q", cmd.Control, want)
		}
	}
}

// Every re-emission must be quiet, because a response to one would arrive on the input path answering a
// question cm never asked. That is the failure interception exists to remove, so re-introducing it here
// would defeat the whole change.
func TestStoreRetransmissionsAreAlwaysQuiet(t *testing.T) {
	for _, raw := range []string{
		"\x1b_Ga=T,i=1;MQ==\x1b\\",     // no q= at all
		"\x1b_Ga=T,q=0,i=2;MQ==\x1b\\", // explicitly noisy
		"\x1b_Ga=T,q=1,i=3;MQ==\x1b\\", // partially quiet
		"\x1b_Ga=T,q=2,i=4;MQ==\x1b\\", // already quiet
	} {
		s := NewStore(0)
		s.Add(mustParse(t, raw))
		for _, r := range s.Retransmissions() {
			cmd := mustParse(t, string(r.Bytes))
			if cmd.Quiet != 2 {
				t.Errorf("re-emission of %q has q=%d, want 2", raw, cmd.Quiet)
			}
		}
	}
}

// A chunked transmission is the normal case, not an edge one: the captured icat run sent one image as
// m=1 chunks. The pieces have to reassemble into one image whose payload is the concatenation.
func TestStoreReassemblesChunks(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T,f=100,s=4,v=3,i=1,m=1;AAAA\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,i=1,m=1;BBBB\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,i=1,m=0;CCCC\x1b\\"))

	if s.Len() != 1 {
		t.Fatalf("Len() = %d, want 1: the chunks describe one image", s.Len())
	}
	got := s.Retransmissions()
	if len(got) != 1 {
		t.Fatalf("Retransmissions() returned %d, want 1", len(got))
	}
	cmd := mustParse(t, string(got[0].Bytes))
	if string(cmd.Payload) != "AAAABBBBCCCC" {
		t.Errorf("reassembled payload = %q, want %q", cmd.Payload, "AAAABBBBCCCC")
	}
	// The control keys come from the first chunk, since later ones carry only m= and q=.
	for _, want := range []string{"s=4", "v=3", "f=100"} {
		if !strings.Contains(cmd.Control, want) {
			t.Errorf("control %q lost %q from the first chunk", cmd.Control, want)
		}
	}
}

// An image still arriving must not be re-emitted. Half a picture is worse than none: a terminal would
// draw something corrupt rather than nothing.
func TestStoreWithholdsIncompleteImages(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T,f=100,i=1,m=1;AAAA\x1b\\"))

	if s.Len() != 1 {
		t.Errorf("Len() = %d, want the partial image retained so later chunks can append", s.Len())
	}
	if got := s.Retransmissions(); len(got) != 0 {
		t.Errorf("Retransmissions() returned %d commands for an incomplete image, want 0", len(got))
	}

	// Once the final chunk lands it becomes available.
	s.Add(mustParse(t, "\x1b_Ga=T,i=1;BBBB\x1b\\"))
	if got := s.Retransmissions(); len(got) != 1 {
		t.Errorf("Retransmissions() returned %d after completion, want 1", len(got))
	}
}

// An id and a number are separate namespaces, so image number 4 and image id 4 are different images.
// Collapsing them would have one transmission silently overwrite the other.
func TestStoreKeepsIDAndNumberApart(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T,i=4;AAAA\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,I=4;BBBB\x1b\\"))

	if s.Len() != 2 {
		t.Fatalf("Len() = %d, want 2: i=4 and I=4 are different images", s.Len())
	}
	byNumber := 0
	for _, r := range s.Retransmissions() {
		if r.ByNumber {
			byNumber++
		}
	}
	if byNumber != 1 {
		t.Errorf("%d retransmissions identified by number, want exactly 1", byNumber)
	}
}

// A command cm cannot name is one it could never re-emit, so it is not worth the bytes.
func TestStoreIgnoresUnidentifiableAndNonTransmissions(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T;AAAA\x1b\\"))     // no id and no number
	s.Add(mustParse(t, "\x1b_Ga=q,i=1;AAAA\x1b\\")) // a query, not a transmission
	s.Add(mustParse(t, "\x1b_Ga=p,i=1\x1b\\"))      // a placement of something already stored
	s.Add(mustParse(t, "\x1b_Ga=d,d=i,i=1\x1b\\"))  // a delete

	if s.Len() != 0 {
		t.Errorf("Len() = %d, want 0: none of those commands transmits a nameable image", s.Len())
	}
}

// Re-transmitting an id replaces its bytes, since the protocol allows reuse and keeping the old ones
// would have a restore draw the previous image.
func TestStoreReplacesAReusedID(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T,i=1;AAAA\x1b\\"))
	before := s.Bytes()
	s.Add(mustParse(t, "\x1b_Ga=T,i=1;BBBBBBBB\x1b\\"))

	if s.Len() != 1 {
		t.Errorf("Len() = %d, want 1", s.Len())
	}
	got := s.Retransmissions()
	cmd := mustParse(t, string(got[0].Bytes))
	if string(cmd.Payload) != "BBBBBBBB" {
		t.Errorf("payload = %q, want the replacement %q", cmd.Payload, "BBBBBBBB")
	}
	// The accounting has to follow, or the store leaks the bytes it dropped.
	if s.Bytes() != len("BBBBBBBB") {
		t.Errorf("Bytes() = %d after replacing %d bytes, want %d",
			s.Bytes(), before, len("BBBBBBBB"))
	}
}

// The bound has to hold whatever a program does, and eviction drops the least recently used.
func TestStoreEvictsLeastRecentlyUsed(t *testing.T) {
	// Room for two payloads of 8 bytes but not three.
	s := NewStore(20)
	s.Add(mustParse(t, "\x1b_Ga=T,i=1;AAAAAAAA\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,i=2;BBBBBBBB\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,i=3;CCCCCCCC\x1b\\"))

	if s.Bytes() > 20 {
		t.Errorf("Bytes() = %d, over the 20 byte limit", s.Bytes())
	}
	ids := map[uint32]bool{}
	for _, r := range s.Retransmissions() {
		ids[r.ID] = true
	}
	if ids[1] {
		t.Error("image 1 survived, want the least recently used evicted")
	}
	if !ids[3] {
		t.Error("image 3 was evicted, want the newest retained")
	}
}

// Displaying a stored image counts as using it, or an image transmitted once and shown repeatedly ages
// out while still on screen, and the restore is missing exactly what the user is looking at.
func TestStoreTouchProtectsAPlacedImage(t *testing.T) {
	s := NewStore(20)
	s.Add(mustParse(t, "\x1b_Ga=T,i=1;AAAAAAAA\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,i=2;BBBBBBBB\x1b\\"))

	// Image 1 is the oldest, but placing it makes it the most recently used.
	s.Touch(1, false)
	s.Add(mustParse(t, "\x1b_Ga=T,i=3;CCCCCCCC\x1b\\"))

	ids := map[uint32]bool{}
	for _, r := range s.Retransmissions() {
		ids[r.ID] = true
	}
	if !ids[1] {
		t.Error("the touched image was evicted, so a placement does not count as use")
	}
	if ids[2] {
		t.Error("image 2 survived, want the now-oldest evicted instead")
	}
}

// A single transmission larger than the whole limit must not wedge the store. Protecting an incomplete
// entry from eviction would do exactly that, since its chunks never stop arriving.
func TestStoreBoundHoldsAgainstAnOversizedTransmission(t *testing.T) {
	s := NewStore(16)
	for i := range 10 {
		s.Add(mustParse(t, fmt.Sprintf("\x1b_Ga=T,i=1,m=1;%s\x1b\\", strings.Repeat("A", 8))))
		if s.Bytes() > 16 {
			t.Fatalf("after chunk %d, Bytes() = %d, over the 16 byte limit", i, s.Bytes())
		}
	}
}

// Explicit deletion and reset have to release the bytes as well as the entries.
func TestStoreDeleteAndReset(t *testing.T) {
	s := NewStore(0)
	s.Add(mustParse(t, "\x1b_Ga=T,i=1;AAAA\x1b\\"))
	s.Add(mustParse(t, "\x1b_Ga=T,i=2;BBBB\x1b\\"))

	s.Delete(1, false)
	if s.Len() != 1 || s.Bytes() != 4 {
		t.Errorf("after Delete: Len() = %d, Bytes() = %d, want 1 and 4", s.Len(), s.Bytes())
	}

	s.Reset()
	if s.Len() != 0 || s.Bytes() != 0 {
		t.Errorf("after Reset: Len() = %d, Bytes() = %d, want 0 and 0", s.Len(), s.Bytes())
	}
	if got := s.Retransmissions(); len(got) != 0 {
		t.Errorf("Retransmissions() returned %d after Reset, want 0", len(got))
	}
}

// Images come back in transmission order, so a terminal receives them as the program sent them.
func TestStoreRetransmissionsAreOldestFirst(t *testing.T) {
	s := NewStore(0)
	for _, id := range []int{3, 1, 2} {
		s.Add(mustParse(t, fmt.Sprintf("\x1b_Ga=T,i=%d;AAAA\x1b\\", id)))
	}
	got := s.Retransmissions()
	want := []uint32{3, 1, 2}
	if len(got) != len(want) {
		t.Fatalf("Retransmissions() returned %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("order = %d at index %d, want %d", got[i].ID, i, want[i])
		}
	}
}
