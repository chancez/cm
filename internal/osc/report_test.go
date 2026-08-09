package osc

import (
	"testing"
)

// The number is part of the wire format, so a change to it breaks every installed shell integration.
// Pinned here so that is a deliberate act rather than an edit nothing notices.
func TestReportNumberIsStable(t *testing.T) {
	if ReportNumber != 25453 {
		t.Errorf("ReportNumber = %d, want 25453 (0x636d, ASCII \"cm\")", ReportNumber)
	}
}

func TestReportTrackerReads(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Report
		found bool
	}{
		{
			name:  "state alone",
			input: "\x1b]25453;state=busy\x07",
			want:  Report{State: "busy"},
			found: true,
		},
		{
			name:  "every field",
			input: "\x1b]25453;state=blocked;detail=needs approval;source=agent\x07",
			want:  Report{State: "blocked", Detail: "needs approval", Source: "agent"},
			found: true,
		},
		{
			name:  "ST terminator",
			input: "\x1b]25453;state=idle\x1b\\",
			want:  Report{State: "idle"},
			found: true,
		},
		{
			name:  "clear withdraws a report",
			input: "\x1b]25453;state=clear\x07",
			want:  Report{State: "clear"},
			found: true,
		},
		{
			name: "fields in any order",
			// The order must not be load-bearing, or adding a field later would be a breaking change.
			input: "\x1b]25453;source=agent;state=busy\x07",
			want:  Report{State: "busy", Source: "agent"},
			found: true,
		},
		{
			name: "unknown keys ignored",
			// An old cm meeting a newer integration keeps what it understands.
			input: "\x1b]25453;state=busy;elapsed=12;future=x\x07",
			want:  Report{State: "busy"},
			found: true,
		},
		{
			name: "escaped semicolon in a value",
			// The escaping exists so a detail can contain a semicolon. Splitting on every semicolon would
			// cut this to "a\" and leave a bogus "b" field, which is the bug commandLine already had to
			// avoid for cmdline.
			input: "\x1b]25453;state=busy;detail=a\\;b\x07",
			want:  Report{State: "busy", Detail: "a;b"},
			found: true,
		},
		{
			name:  "unknown state rejected",
			input: "\x1b]25453;state=wat\x07",
			want:  Report{},
			found: false,
		},
		{
			name: "no state is not a report",
			// Detail without a state says nothing that can be waited for.
			input: "\x1b]25453;detail=something\x07",
			want:  Report{},
			found: false,
		},
		{
			name:  "ordinary output",
			input: "just some text\r\n",
			want:  Report{},
			found: false,
		},
		{
			name: "another OSC is not ours",
			// OSC 133 travels in the same stream, so the two must not be confused.
			input: "\x1b]133;C\x07",
			want:  Report{},
			found: false,
		},
		{
			name: "a longer number is not ours",
			// The introducer must match on the full number: a prefix match would claim OSC 254530.
			input: "\x1b]254530;state=busy\x07",
			want:  Report{},
			found: false,
		},
		{
			name: "last one wins",
			// Several in one chunk describe the same shell, so the newest is the truth.
			input: "\x1b]25453;state=busy\x07out\x1b]25453;state=idle\x07",
			want:  Report{State: "idle"},
			found: true,
		},
		{
			name:  "surrounded by output",
			input: "before\x1b]25453;state=busy\x07after",
			want:  Report{State: "busy"},
			found: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr ReportTracker
			if got := tr.Feed([]byte(tt.input)); got != tt.found {
				t.Errorf("Feed() = %v, want %v", got, tt.found)
			}
			got, ok := tr.Take()
			if ok != tt.found {
				t.Fatalf("Take() found = %v, want %v", ok, tt.found)
			}
			if got != tt.want {
				t.Errorf("Take() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A sequence split across reads must still be recognized.
//
// This is why the tracker is stateful. A pty read is bounded by the kernel buffer rather than by anything
// the shell intends, so a report can be cut at any byte, including inside the introducer. Every split
// point is exercised rather than a chosen few, because the interesting ones are exactly the boundaries a
// hand-picked case would miss.
func TestReportTrackerHandlesEverySplit(t *testing.T) {
	const seq = "\x1b]25453;state=blocked;detail=waiting\x07"
	input := "before " + seq + " after"
	want := Report{State: "blocked", Detail: "waiting"}

	for i := 1; i < len(input); i++ {
		var tr ReportTracker
		first := tr.Feed([]byte(input[:i]))
		second := tr.Feed([]byte(input[i:]))
		if !first && !second {
			t.Fatalf("split at %d: no report found in %q + %q", i, input[:i], input[i:])
		}
		got, ok := tr.Take()
		if !ok {
			t.Fatalf("split at %d: Take() found nothing", i)
		}
		if got != want {
			t.Errorf("split at %d: Take() = %+v, want %+v", i, got, want)
		}
	}
}

// Take drains, because a report is an event to forward once rather than a value to re-read.
func TestReportTrackerTakeDrains(t *testing.T) {
	var tr ReportTracker
	tr.Feed([]byte("\x1b]25453;state=busy\x07"))

	if _, ok := tr.Take(); !ok {
		t.Fatal("first Take() found nothing")
	}
	if got, ok := tr.Take(); ok {
		t.Errorf("second Take() = %+v, want nothing", got)
	}
}

// A malformed report must not erase a valid one, or a shell emitting nonsense could clear real state.
func TestReportTrackerKeepsPreviousOnMalformed(t *testing.T) {
	var tr ReportTracker
	tr.Feed([]byte("\x1b]25453;state=busy\x07"))
	if got := tr.Feed([]byte("\x1b]25453;state=nonsense\x07")); got {
		t.Error("Feed() reported a malformed sequence as a report")
	}

	got, ok := tr.Take()
	if !ok {
		t.Fatal("Take() found nothing, want the earlier valid report")
	}
	if want := (Report{State: "busy"}); got != want {
		t.Errorf("Take() = %+v, want %+v", got, want)
	}
}

// An unterminated sequence must not grow the held-back buffer without bound.
func TestReportTrackerBoundsPartial(t *testing.T) {
	var tr ReportTracker
	tr.Feed([]byte("\x1b]25453;state=busy"))
	big := make([]byte, maxPartial*2)
	for i := range big {
		big[i] = 'x'
	}
	tr.Feed(big)

	if len(tr.partial) > maxPartial {
		t.Errorf("partial grew to %d bytes, want at most %d", len(tr.partial), maxPartial)
	}
}
