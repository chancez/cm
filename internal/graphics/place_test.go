package graphics

import (
	"reflect"
	"testing"
)

// A placement is restored as a position plus a=p, never as a=T, and the framing around it is what keeps
// a restore from disturbing the screen it just drew.
func TestPlaceCommandsRebuildsAPlacement(t *testing.T) {
	got := PlaceCommands([]Placement{{
		ImageID:     7,
		PlacementID: 3,
		Col:         4,
		Row:         2,
		Columns:     10,
		Rows:        5,
		Z:           -1,
	}})

	want := "\x1b7" + // save the cursor, so the restored screen keeps its own
		"\x1b[3;5H" + // one-based CUP for the zero-based row 2, column 4
		"\x1b_Ga=p,i=7,p=3,c=10,r=5,z=-1,C=1,q=2\x1b\\" +
		"\x1b8" // put the cursor back
	if string(got) != want {
		t.Errorf("PlaceCommands() = %q, want %q", got, want)
	}
}

// Nothing placed emits nothing, which is the common case: no save, no restore, no bytes.
func TestPlaceCommandsIsEmptyWithoutPlacements(t *testing.T) {
	if got := PlaceCommands(nil); got != nil {
		t.Errorf("PlaceCommands(nil) = %q, want nil", got)
	}
}

// The keys a placement does not carry are omitted rather than sent as zero. c=0 or r=0 would tell the
// terminal the image covers no cells, and z=0 is the default.
func TestPlaceCommandsOmitsAbsentKeys(t *testing.T) {
	got := PlaceCommands([]Placement{{ImageID: 1}})
	want := "\x1b7\x1b[1;1H\x1b_Ga=p,i=1,C=1,q=2\x1b\\\x1b8"
	if string(got) != want {
		t.Errorf("PlaceCommands() = %q, want %q", got, want)
	}
}

// C=1 is on every command, and it is not cosmetic: without it a placement on the last row advances the
// cursor and scrolls the whole screen up to make room, which moves the content the restore just wrote.
func TestPlaceCommandsNeverMovesTheCursor(t *testing.T) {
	got := string(PlaceCommands([]Placement{{ImageID: 1, Row: 0}, {ImageID: 2, Row: 40}}))
	for _, want := range []string{"i=1,C=1", "i=2,C=1"} {
		if !contains(got, want) {
			t.Errorf("PlaceCommands() = %q, missing %q", got, want)
		}
	}
}

// A placement whose top has scrolled above the viewport is skipped rather than drawn at row zero, which
// would put the image somewhere it never was. Restoring it properly means cropping with a source
// rectangle, which libghostty documents as the caller's job.
func TestPlaceCommandsSkipsAPlacementScrolledAbove(t *testing.T) {
	got := PlaceCommands([]Placement{{ImageID: 9, Row: -2, Col: 0}})
	if got != nil {
		t.Errorf("PlaceCommands() = %q, want nil for a placement above the viewport", got)
	}
}

// An unnamed image cannot be placed, since a=p resolves by id.
func TestPlaceCommandsSkipsAnUnnamedImage(t *testing.T) {
	if got := PlaceCommands([]Placement{{ImageID: 0, Row: 1}}); got != nil {
		t.Errorf("PlaceCommands() = %q, want nil", got)
	}
}

// Several placements share one save/restore pair, so the cursor is put back once at the end.
func TestPlaceCommandsWrapsAllPlacementsOnce(t *testing.T) {
	got := string(PlaceCommands([]Placement{{ImageID: 1}, {ImageID: 2}}))
	if n := count(got, "\x1b7"); n != 1 {
		t.Errorf("%d cursor saves, want 1", n)
	}
	if n := count(got, "\x1b8"); n != 1 {
		t.Errorf("%d cursor restores, want 1", n)
	}
	// And the parsed commands are the two placements, in order.
	var ids []uint32
	rest := []byte(got)
	for len(rest) > 0 {
		i := indexOf(rest, "\x1b_G")
		if i < 0 {
			break
		}
		cmd, n, ok := Parse(rest[i:])
		if !ok {
			t.Fatalf("a placement command does not parse: %q", rest[i:])
		}
		ids = append(ids, cmd.ImageID)
		rest = rest[i+n:]
	}
	if want := []uint32{1, 2}; !reflect.DeepEqual(ids, want) {
		t.Errorf("placed image ids = %v, want %v", ids, want)
	}
}

func contains(s, sub string) bool { return indexOf([]byte(s), sub) >= 0 }

func count(s, sub string) int {
	n := 0
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			n++
		}
	}
	return n
}

func indexOf(b []byte, sub string) int {
	for i := 0; i+len(sub) <= len(b); i++ {
		if string(b[i:i+len(sub)]) == sub {
			return i
		}
	}
	return -1
}
