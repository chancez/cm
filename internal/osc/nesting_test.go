package osc

import (
	"reflect"
	"strings"
	"testing"
)

// The announcement is a protocol between two cm processes that are usually different builds, since the
// far side of an ssh upgrades on its own schedule. Pinned so a change to the spelling is deliberate.
func TestNestingSequenceIsStable(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		session string
		ended   bool
		want    string
	}{
		{name: "begin", id: "9f3c2a", want: "\x1b]25453;client=begin;id=9f3c2a\a"},
		{name: "end", id: "9f3c2a", ended: true, want: "\x1b]25453;client=end;id=9f3c2a\a"},
		{
			// The session a client attached to, which is what makes a parent able to say where a window went.
			name: "begin with a session", id: "9f3c2a", session: "books",
			want: "\x1b]25453;client=begin;id=9f3c2a;session=books\a",
		},
		{
			// Escaped, since a semicolon separates fields on the wire. A name cannot contain one, but a
			// reference is a string this process was handed rather than one it validated.
			name: "begin with a session needing escapes", id: "9f3c2a", session: "a;b\\c",
			want: "\x1b]25453;client=begin;id=9f3c2a;session=a\\;b\\\\c\a",
		},
		{
			// Dropped on an end, where the id is what pairs the withdrawal with the announcement.
			name: "end ignores the session", id: "9f3c2a", session: "books", ended: true,
			want: "\x1b]25453;client=end;id=9f3c2a\a",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := string(NestingSequence(tt.id, tt.session, tt.ended)); got != tt.want {
				t.Errorf("NestingSequence(%q, %q, %v) = %q, want %q", tt.id, tt.session, tt.ended, got, tt.want)
			}
		})
	}
}

// What cm emits must be what cm reads. Separate from the pinned spelling above, which would still pass
// if the reader disagreed with the writer.
func TestNestingSequenceRoundTrips(t *testing.T) {
	var tr ReportTracker
	tr.Feed(NestingSequence("abc123", "a;b", false))
	tr.Feed(NestingSequence("abc123", "", true))

	want := []Nesting{{ID: "abc123", Session: "a;b"}, {ID: "abc123", Ended: true}}
	if got := tr.TakeNesting(); !reflect.DeepEqual(got, want) {
		t.Errorf("TakeNesting() = %+v, want %+v", got, want)
	}
}

func TestReportTrackerReadsNesting(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Nesting
	}{
		{
			name:  "begin",
			input: "\x1b]25453;client=begin;id=9f3c2a\x07",
			want:  []Nesting{{ID: "9f3c2a"}},
		},
		{
			name:  "end",
			input: "\x1b]25453;client=end;id=9f3c2a\x07",
			want:  []Nesting{{ID: "9f3c2a", Ended: true}},
		},
		{
			name:  "ST terminator",
			input: "\x1b]25453;client=begin;id=9f3c2a\x1b\\",
			want:  []Nesting{{ID: "9f3c2a"}},
		},
		{
			name:  "fields in any order",
			input: "\x1b]25453;id=9f3c2a;client=begin\x07",
			want:  []Nesting{{ID: "9f3c2a"}},
		},
		{
			name: "session named",
			// The label a parent needs to say where a window went, since the id is a nonce.
			input: "\x1b]25453;client=begin;id=9f3c2a;session=books\x07",
			want:  []Nesting{{ID: "9f3c2a", Session: "books"}},
		},
		{
			name:  "session with an escaped separator",
			input: "\x1b]25453;client=begin;id=9f3c2a;session=a\\;b\x07",
			want:  []Nesting{{ID: "9f3c2a", Session: "a;b"}},
		},
		{
			name: "session ID as the reference",
			// What `cm tui` announces before its Open answers, since the picker attaches by ID.
			input: "\x1b]25453;client=begin;id=9f3c2a;session=@a7k2m9x4\x07",
			want:  []Nesting{{ID: "9f3c2a", Session: "@a7k2m9x4"}},
		},
		{
			name: "control bytes are stripped from the session",
			// These bytes can come from anything that prints, and this value travels into `cm info` output.
			// A BEL is not among them and must not be tested for: it terminates the sequence, so a value
			// containing one is truncated there rather than carried and stripped.
			input: "\x1b]25453;client=begin;id=9f3c2a;session=bo\x01ok\x7fs\x07",
			want:  []Nesting{{ID: "9f3c2a", Session: "books"}},
		},
		{
			name:  "an over-long session is truncated rather than dropped",
			input: "\x1b]25453;client=begin;id=9f3c2a;session=" + strings.Repeat("n", maxNestingSession+20) + "\x07",
			want:  []Nesting{{ID: "9f3c2a", Session: strings.Repeat("n", maxNestingSession)}},
		},
		{
			name: "unknown keys ignored",
			// An older parent meeting a newer client keeps what it understands, which is the same
			// tolerance parseReport documents.
			input: "\x1b]25453;client=begin;id=9f3c2a;host=white;future=x\x07",
			want:  []Nesting{{ID: "9f3c2a"}},
		},
		{
			name: "a begin and its end in one chunk are both kept",
			// Collapsing these last-one-wins, the way a report collapses, would leave the parent
			// believing a client that has already gone is still there.
			input: "\x1b]25453;client=begin;id=a1\x07out\x1b]25453;client=end;id=a1\x07",
			want:  []Nesting{{ID: "a1"}, {ID: "a1", Ended: true}},
		},
		{
			name: "two clients are distinct",
			// An ssh chain: the second hop's client announces through the first's pty as well.
			input: "\x1b]25453;client=begin;id=a1\x07\x1b]25453;client=begin;id=b2\x07",
			want:  []Nesting{{ID: "a1"}, {ID: "b2"}},
		},
		{
			name:  "no id is not an announcement",
			input: "\x1b]25453;client=begin\x07",
			want:  nil,
		},
		{
			name:  "empty id is not an announcement",
			input: "\x1b]25453;client=begin;id=\x07",
			want:  nil,
		},
		{
			name: "an id outside the allowed set is rejected",
			// The value becomes a key in the parent's map and these bytes can come from anything that
			// prints, so arbitrary text must not get there.
			input: "\x1b]25453;client=begin;id=../etc/passwd\x07",
			want:  nil,
		},
		{
			name:  "an unknown client state is not an announcement",
			input: "\x1b]25453;client=maybe;id=a1\x07",
			want:  nil,
		},
		{
			name:  "a report is not an announcement",
			input: "\x1b]25453;state=busy\x07",
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tr ReportTracker
			tr.Feed([]byte(tt.input))
			if got := tr.TakeNesting(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TakeNesting() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A pty read is bounded by the kernel buffer rather than by anything the writer intends, so an
// announcement split across reads has to survive. This is the case the partial holding exists for, and
// the one a stateless matcher drops.
func TestNestingSplitAcrossFeeds(t *testing.T) {
	seq := "\x1b]25453;client=begin;id=9f3c2a\x07"
	for cut := 1; cut < len(seq); cut++ {
		var tr ReportTracker
		tr.Feed([]byte(seq[:cut]))
		got := tr.TakeNesting()
		if got != nil {
			t.Errorf("cut %d: TakeNesting() before the rest arrived = %+v, want nil", cut, got)
		}
		tr.Feed([]byte(seq[cut:]))
		want := []Nesting{{ID: "9f3c2a"}}
		if got := tr.TakeNesting(); !reflect.DeepEqual(got, want) {
			t.Errorf("cut %d: TakeNesting() = %+v, want %+v", cut, got, want)
		}
	}
}

// Draining is what makes each announcement apply once. Without it the parent would re-register the same
// client on every later chunk of output.
func TestTakeNestingDrains(t *testing.T) {
	var tr ReportTracker
	tr.Feed([]byte("\x1b]25453;client=begin;id=a1\x07"))
	if got := tr.TakeNesting(); len(got) != 1 {
		t.Fatalf("first TakeNesting() = %+v, want one announcement", got)
	}
	if got := tr.TakeNesting(); got != nil {
		t.Errorf("second TakeNesting() = %+v, want nil", got)
	}
}

// A report and an announcement share the introducer, so one must not consume the other.
func TestNestingAndReportsCoexist(t *testing.T) {
	var tr ReportTracker
	found := tr.Feed([]byte("\x1b]25453;client=begin;id=a1\x07\x1b]25453;state=busy;source=zsh\x07"))
	if !found {
		t.Error("Feed() = false, want true: the report in the same chunk was missed")
	}
	wantReport := Report{State: "busy", Source: "zsh"}
	if got, ok := tr.Take(); !ok || got != wantReport {
		t.Errorf("Take() = %+v, %v, want %+v, true", got, ok, wantReport)
	}
	wantNesting := []Nesting{{ID: "a1"}}
	if got := tr.TakeNesting(); !reflect.DeepEqual(got, wantNesting) {
		t.Errorf("TakeNesting() = %+v, want %+v", got, wantNesting)
	}
}

// Announcements are bytes in a session's output, so anything that prints them is a source: `cat` of a
// file holding one is the honest case. Bounded so such a stream cannot grow the queue without limit.
func TestPendingNestingIsBounded(t *testing.T) {
	var tr ReportTracker
	for i := 0; i < maxPendingNesting*3; i++ {
		tr.Feed(NestingSequence("a1", "", false))
	}
	if got := len(tr.TakeNesting()); got != maxPendingNesting {
		t.Errorf("pending announcements = %d, want %d", got, maxPendingNesting)
	}
}
