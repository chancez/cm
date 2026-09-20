package osc

import (
	"reflect"
	"strings"
	"testing"
)

// The spelling is a contract with a shell integration a user may have loaded from an older build, so a
// change to it is deliberate rather than an edit nothing notices.
func TestFrameSequenceIsStable(t *testing.T) {
	tests := []struct {
		name  string
		id    string
		argv  string
		ended bool
		want  string
	}{
		{
			name: "enter",
			id:   "7f2a-1",
			argv: "kitten ssh white",
			want: "\x1b]25453;frame=enter;id=7f2a-1;argv=kitten ssh white\a",
		},
		{
			name:  "exit carries no argv",
			id:    "7f2a-1",
			argv:  "kitten ssh white",
			ended: true,
			want:  "\x1b]25453;frame=exit;id=7f2a-1\a",
		},
		{
			name: "a semicolon in the command is escaped",
			// Unescaped it would split the payload and the rest of the command would be read as another
			// field, which is how a value with a separator in it silently truncates.
			id:   "7f2a-2",
			argv: "ssh white; echo done",
			want: "\x1b]25453;frame=enter;id=7f2a-2;argv=ssh white\\; echo done\a",
		},
		{
			name: "a backslash is escaped",
			id:   "7f2a-3",
			argv: `grep \; file`,
			want: "\x1b]25453;frame=enter;id=7f2a-3;argv=grep \\\\\\; file\a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(FrameSequence(tt.id, tt.argv, tt.ended)); got != tt.want {
				t.Errorf("FrameSequence() = %q, want %q", got, tt.want)
			}
		})
	}
}

// What cm writes must be what cm reads, which the pinned spelling above would not catch on its own: a
// reader that disagreed with the writer would still pass it.
func TestFrameSequenceRoundTrips(t *testing.T) {
	for _, argv := range []string{
		"kitten ssh white",
		"ssh white; echo done",
		`grep \; file`,
		"ssh -J jump white 'cm attach books'",
	} {
		var tr ReportTracker
		tr.Feed(FrameSequence("a1", argv, false))
		tr.Feed(FrameSequence("a1", argv, true))

		want := []Frame{{ID: "a1", Argv: argv}, {ID: "a1", Ended: true}}
		if got := tr.TakeFrames(); !reflect.DeepEqual(got, want) {
			t.Errorf("argv %q: TakeFrames() = %+v, want %+v", argv, got, want)
		}
	}
}

func TestReportTrackerReadsFrames(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Frame
	}{
		{
			name:  "enter with a command",
			input: "\x1b]25453;frame=enter;id=a1;argv=kitten ssh white\x07",
			want:  []Frame{{ID: "a1", Argv: "kitten ssh white"}},
		},
		{
			name:  "exit",
			input: "\x1b]25453;frame=exit;id=a1\x07",
			want:  []Frame{{ID: "a1", Ended: true}},
		},
		{
			name:  "fields in any order",
			input: "\x1b]25453;argv=ssh white;id=a1;frame=enter\x07",
			want:  []Frame{{ID: "a1", Argv: "ssh white"}},
		},
		{
			name: "unknown keys ignored",
			// An older cm meeting a newer integration keeps what it understands, the same tolerance
			// parseReport documents.
			input: "\x1b]25453;frame=enter;id=a1;argv=ssh white;host=white;future=x\x07",
			want:  []Frame{{ID: "a1", Argv: "ssh white"}},
		},
		{
			name: "a command with no argv is still a frame",
			// The argv is what a consumer wants and not what the stack needs: a frame with no command is
			// still a frame, and dropping it would leave an exit with nothing to close.
			input: "\x1b]25453;frame=enter;id=a1\x07",
			want:  []Frame{{ID: "a1"}},
		},
		{
			name: "an open and its close in one chunk are both kept",
			// A fast command produces both between two reads. Collapsing them would leave a frame open
			// forever, which for the collector means an announcement that is never discarded.
			input: "\x1b]25453;frame=enter;id=a1;argv=true\x07out\x1b]25453;frame=exit;id=a1\x07",
			want:  []Frame{{ID: "a1", Argv: "true"}, {ID: "a1", Ended: true}},
		},
		{
			name:  "no id is not a frame",
			input: "\x1b]25453;frame=enter;argv=ssh white\x07",
			want:  nil,
		},
		{
			name: "an id outside the allowed set is rejected",
			// The id becomes a key, and these bytes can come from anything that prints.
			input: "\x1b]25453;frame=enter;id=../../etc;argv=ssh\x07",
			want:  nil,
		},
		{
			name:  "an unknown frame state is not a frame",
			input: "\x1b]25453;frame=maybe;id=a1\x07",
			want:  nil,
		},
		{
			name:  "a report is not a frame",
			input: "\x1b]25453;state=busy\x07",
			want:  nil,
		},
		{
			name:  "an announcement is not a frame",
			input: "\x1b]25453;client=begin;id=a1\x07",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr ReportTracker
			tr.Feed([]byte(tt.input))
			if got := tr.TakeFrames(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TakeFrames() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A command line arrives from a shell and ends up in `cm list --json`, so what it may contain is bounded
// here rather than trusted. A newline would break a line-oriented consumer and a control byte would travel
// out to whatever reads the JSON.
func TestFrameArgvIsSanitized(t *testing.T) {
	tests := []struct {
		name string
		argv string
		want string
	}{
		{name: "newlines are dropped", argv: "ssh white\nrm -rf /", want: "ssh whiterm -rf /"},
		{name: "carriage returns are dropped", argv: "ssh\rwhite", want: "sshwhite"},
		{name: "escape is dropped", argv: "ssh \x1b[31mwhite", want: "ssh [31mwhite"},
		{name: "tabs become spaces", argv: "ssh\twhite", want: "ssh white"},
		{name: "printable text survives", argv: "ssh -J jump white", want: "ssh -J jump white"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr ReportTracker
			tr.Feed(FrameSequence("a1", tt.argv, false))
			got := tr.TakeFrames()
			if len(got) != 1 || got[0].Argv != tt.want {
				t.Errorf("TakeFrames() = %+v, want one frame with argv %q", got, tt.want)
			}
		})
	}
}

func TestFrameArgvIsBounded(t *testing.T) {
	var tr ReportTracker
	tr.Feed(FrameSequence("a1", strings.Repeat("x", maxFrameArgv*3), false))

	got := tr.TakeFrames()
	if len(got) != 1 || len(got[0].Argv) != maxFrameArgv {
		t.Errorf("argv length = %d, want %d", len(got[0].Argv), maxFrameArgv)
	}
}

// All three payloads share the introducer, so none may consume another. The frame is the one added last,
// which makes this the case most likely to break quietly.
func TestFramesReportsAndNestingCoexist(t *testing.T) {
	var tr ReportTracker
	found := tr.Feed([]byte(
		"\x1b]25453;frame=enter;id=f1;argv=kitten ssh white\x07" +
			"\x1b]25453;client=begin;id=c1\x07" +
			"\x1b]25453;state=blocked;detail=waiting\x07"))

	if !found {
		t.Error("Feed() = false, want true: the report in the same chunk was missed")
	}
	wantReport := Report{State: "blocked", Detail: "waiting"}
	if got, ok := tr.Take(); !ok || got != wantReport {
		t.Errorf("Take() = %+v, %v, want %+v, true", got, ok, wantReport)
	}
	if got, want := tr.TakeNesting(), []Nesting{{ID: "c1"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("TakeNesting() = %+v, want %+v", got, want)
	}
	if got, want := tr.TakeFrames(), []Frame{{ID: "f1", Argv: "kitten ssh white"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("TakeFrames() = %+v, want %+v", got, want)
	}
}

// A pty read ends where the kernel buffer does, so a frame arrives split. This is what the partial holding
// exists for, and the case a stateless matcher drops.
func TestFrameSplitAcrossFeeds(t *testing.T) {
	seq := string(FrameSequence("a1", "kitten ssh white", false))
	for cut := 1; cut < len(seq); cut++ {
		var tr ReportTracker
		tr.Feed([]byte(seq[:cut]))
		if got := tr.TakeFrames(); got != nil {
			t.Errorf("cut %d: TakeFrames() before the rest arrived = %+v, want nil", cut, got)
		}
		tr.Feed([]byte(seq[cut:]))
		want := []Frame{{ID: "a1", Argv: "kitten ssh white"}}
		if got := tr.TakeFrames(); !reflect.DeepEqual(got, want) {
			t.Errorf("cut %d: TakeFrames() = %+v, want %+v", cut, got, want)
		}
	}
}

// Draining is what makes each frame apply once, without which the stack would grow a copy of the open
// frame on every later chunk of output.
func TestTakeFramesDrains(t *testing.T) {
	var tr ReportTracker
	tr.Feed(FrameSequence("a1", "ssh white", false))
	if got := tr.TakeFrames(); len(got) != 1 {
		t.Fatalf("first TakeFrames() = %+v, want one frame", got)
	}
	if got := tr.TakeFrames(); got != nil {
		t.Errorf("second TakeFrames() = %+v, want nil", got)
	}
}

// Frames arrive as ordinary output, so anything that prints them is a source. Bounded for the same reason
// announcements are.
func TestPendingFramesAreBounded(t *testing.T) {
	var tr ReportTracker
	for i := 0; i < maxPendingNesting*3; i++ {
		tr.Feed(FrameSequence("a1", "ssh white", false))
	}
	if got := len(tr.TakeFrames()); got != maxPendingNesting {
		t.Errorf("pending frames = %d, want %d", got, maxPendingNesting)
	}
}
