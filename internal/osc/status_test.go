package osc

import (
	"slices"
	"strings"
	"testing"
)

// A report split across reads is still read, at every split point.
//
// A pty read ends wherever the kernel buffer does, so a report at a chunk boundary is routine, and a
// tracker that only matched whole sequences would lose exactly those.
func TestStatusTrackerReadsASplitReport(t *testing.T) {
	stream := "before" + string(StatusSequence(Status{State: "working", App: "cargo", Message: "building", Progress: 40})) + "after"
	want := []Status{{State: "working", App: "cargo", Message: "building", Progress: 40}}
	for i := 1; i < len(stream); i++ {
		var tr StatusTracker
		tr.Feed([]byte(stream[:i]))
		tr.Feed([]byte(stream[i:]))
		if got := tr.Take(); !slices.Equal(got, want) {
			t.Fatalf("split at %d: Take() = %+v, want %+v", i, got, want)
		}
	}
}

// Reports in one chunk come out in order, none collapsed.
//
// Unlike cm's own report, which describes one value, these are about records: two reports in one chunk can
// be about two different things, and a clear only means something relative to what came before it.
func TestStatusTrackerKeepsEveryReportInOrder(t *testing.T) {
	var tr StatusTracker
	tr.Feed([]byte("\x1b]7501;state=working:id=a\x07x\x1b]7501;state=bogus\x07\x1b]7501;state=done:id=b\x1b\\\x1b]7501;state=clear:id=a\x07"))
	want := []Status{
		{State: "working", ID: "a", Progress: -1},
		{State: "done", ID: "b", Progress: -1},
		{State: "clear", ID: "a", Progress: -1},
	}
	if got := tr.Take(); !slices.Equal(got, want) {
		t.Errorf("Take() = %+v, want %+v", got, want)
	}
	if got := tr.Take(); got != nil {
		t.Errorf("second Take() = %+v, want nil", got)
	}
}

// Output that only looks like the start of a report is not held forever.
func TestStatusTrackerBoundsItsPartial(t *testing.T) {
	var tr StatusTracker
	tr.Feed([]byte(statusIntro + strings.Repeat("x", maxPartial*2)))
	if len(tr.partial) > maxPartial {
		t.Errorf("partial grew to %d bytes, want at most %d", len(tr.partial), maxPartial)
	}
}

// What StatusSequence writes, ParseStatus reads back unchanged.
func TestStatusSequenceRoundTrips(t *testing.T) {
	for _, s := range []Status{
		{State: "idle", Progress: -1},
		{State: "blocked", Kind: "permission", Progress: 10, ID: "deploy/eu-west", App: "terraform", Title: "EU West", Message: "Apply?"},
		{State: "done", Progress: -1, Message: "semi;colon: and \\ backslash"},
	} {
		seq := StatusSequence(s)
		got, ok := ParseStatus(seq[len(statusIntro) : len(seq)-1])
		if !ok || got != s {
			t.Errorf("ParseStatus(StatusSequence(%+v)) = %+v, %v", s, got, ok)
		}
	}
}
