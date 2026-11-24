package termmodes

import (
	"reflect"
	"testing"
)

func TestTrackerKeepsWhereAnUnchangedModeWasSet(t *testing.T) {
	tr := NewTracker(nil)
	if !tr.Observe(map[string]int{AltScreen: 1, "?1003": 1}, 100) {
		t.Fatal("setting two modes reported no change")
	}
	// gr redraws for days with both still set. Each chunk that may have touched a mode is observed again,
	// and neither keeps the position of that later chunk.
	if tr.Observe(map[string]int{AltScreen: 1, "?1003": 1}, 5000) {
		t.Fatal("observing the same modes reported a change")
	}
	if !tr.Observe(map[string]int{AltScreen: 1, "?1003": 1, KittyKeyboard: 1}, 9000) {
		t.Fatal("pushing kitty keyboard flags reported no change")
	}
	want := []Mode{
		{Key: "?1003", Value: 1, SetAt: 100},
		{Key: AltScreen, Value: 1, SetAt: 100},
		{Key: KittyKeyboard, Value: 1, SetAt: 9000},
	}
	if got := tr.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %+v, want %+v", got, want)
	}
}

func TestTrackerDropsAModeBackAtItsDefault(t *testing.T) {
	tr := NewTracker([]Mode{{Key: AltScreen, Value: 1, SetAt: 100}, {Key: "?1003", Value: 1, SetAt: 100}})
	if !tr.Observe(map[string]int{"?1003": 1}, 700) {
		t.Fatal("leaving the alternate screen reported no change")
	}
	want := []Mode{{Key: "?1003", Value: 1, SetAt: 100}}
	if got := tr.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %+v, want %+v", got, want)
	}
}

func TestTrackerRecordsAChangedValue(t *testing.T) {
	tr := NewTracker([]Mode{{Key: KittyKeyboard, Value: 1, SetAt: 100}})
	tr.Observe(map[string]int{KittyKeyboard: 3}, 200)
	want := []Mode{{Key: KittyKeyboard, Value: 3, SetAt: 200}}
	if got := tr.Modes(); !reflect.DeepEqual(got, want) {
		t.Fatalf("modes = %+v, want %+v", got, want)
	}
}

func TestTrimmedIsWhatTheReplayCannotSet(t *testing.T) {
	recorded := []Mode{
		{Key: "?1003", Value: 1, SetAt: 11435653},
		{Key: AltScreen, Value: 1, SetAt: 11435653},
		// Set inside what the shim still holds, so the replay sets it where it changed.
		{Key: "?2004", Value: 1, SetAt: 16000000},
		// A chunk starting exactly at the oldest byte is still retained whole.
		{Key: "?25", Value: 0, SetAt: 12400000},
	}
	want := []Mode{
		{Key: "?1003", Value: 1, SetAt: 11435653},
		{Key: AltScreen, Value: 1, SetAt: 11435653},
	}
	if got := Trimmed(recorded, 12400000); !reflect.DeepEqual(got, want) {
		t.Fatalf("Trimmed = %+v, want %+v", got, want)
	}
}

func TestFormatSpellsEachModeAsItsSequence(t *testing.T) {
	got := Format([]Mode{
		{Key: "?1003", Value: 1},
		{Key: AltScreen, Value: 1},
		{Key: "?25", Value: 0},
		{Key: KittyKeyboard, Value: 1},
	})
	if want := "?1003h ?1049h ?25l kitty_keyboard=1"; got != want {
		t.Fatalf("Format = %q, want %q", got, want)
	}
}

func TestWatchMayChange(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want []bool
	}{
		{"text", []string{"hello world\r\n"}, []bool{false}},
		{"cursor motion and colour", []string{"\x1b[2J\x1b[H\x1b[38;5;4mx\x1b[0m"}, []bool{false}},
		{"dec private set", []string{"\x1b[?1049h"}, []bool{true}},
		{"dec private reset", []string{"ab\x1b[?1003l"}, []bool{true}},
		{"ansi insert mode", []string{"\x1b[4h"}, []bool{true}},
		{"kitty push", []string{"\x1b[>1u"}, []bool{true}},
		{"kitty pop", []string{"\x1b[<u"}, []bool{true}},
		{"kitty set", []string{"\x1b[=1;1u"}, []bool{true}},
		{"ris", []string{"\x1bc"}, []bool{true}},
		{"decstr", []string{"\x1b[!p"}, []bool{true}},
		// The pump holds a partial tail back, but the adoption replay reads the shim's chunks as they were
		// stored, and a mode sequence across their boundary must still be seen.
		{"split after esc", []string{"x\x1b", "[?1049h"}, []bool{false, true}},
		{"split after csi", []string{"x\x1b[", "?1003h"}, []bool{false, true}},
		{"split in params", []string{"\x1b[20", "h"}, []bool{false, true}},
		{"split non mode", []string{"\x1b[3", "8;5;1m"}, []bool{false, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var w Watch
			got := make([]bool, len(tc.in))
			for i, p := range tc.in {
				got[i] = w.MayChange([]byte(p))
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MayChange = %v, want %v", got, tc.want)
			}
		})
	}
}
