package osc

import (
	"reflect"
	"strings"
	"testing"
)

// prompt returns a 133;A marker as a shell emits it.
func promptSeq() string { return "\x1b]133;A\x07" }

// cmdSeq returns a 133;C marker carrying a command line.
func cmdSeq(cmd string) string { return "\x1b]133;C;cmdline=" + cmd + "\x07" }

// doneSeq returns a 133;D marker with an exit status.
func doneSeq(code string) string { return "\x1b]133;D;" + code + "\x07" }

// A single command's block records both positions and the command line.
func TestBoundaryTrackerRecordsOneBlock(t *testing.T) {
	tr := NewBoundaryTracker(0)

	// Fed as one chunk, so the expected positions are simple offsets into it.
	stream := promptSeq() + "$ make\n" + cmdSeq("make") + "output\n" + doneSeq("0")
	tr.Feed([]byte(stream))

	want := []Boundary{{
		PromptSeq: 0,
		HasPrompt: true,
		// Just past the C marker's terminator: the prompt, the echoed line, and the marker itself all
		// precede the command's own output.
		OutputSeq: uint64(len(promptSeq()) + len("$ make\n") + len(cmdSeq("make"))),
		HasOutput: true,
		Command:   "make",
	}}
	if got := tr.Blocks(); !reflect.DeepEqual(got, want) {
		t.Errorf("Blocks() = %+v\nwant %+v", got, want)
	}
}

// Positions must be correct when a marker is split across two feeds.
//
// A pty read is bounded by the kernel buffer rather than by anything the shell intends, so a marker
// straddling two chunks is normal. The trap is recording the position of the *second* fragment: the
// marker began in the previous chunk, so a naive implementation reports a position several bytes too
// high and every later read starts mid-sequence.
func TestBoundaryTrackerHandlesSplitMarkers(t *testing.T) {
	full := promptSeq() + "$ ls\n" + cmdSeq("ls") + "a b c\n"

	// Every possible split point, so no single lucky boundary passes for the wrong reason.
	for cut := 1; cut < len(full); cut++ {
		tr := NewBoundaryTracker(0)
		tr.Feed([]byte(full[:cut]))
		tr.Feed([]byte(full[cut:]))

		blocks := tr.Blocks()
		if len(blocks) != 1 {
			t.Fatalf("split at %d: got %d blocks, want 1: %+v", cut, len(blocks), blocks)
		}
		got := blocks[0]
		wantOutput := uint64(len(promptSeq()) + len("$ ls\n") + len(cmdSeq("ls")))
		if got.PromptSeq != 0 || !got.HasPrompt {
			t.Errorf("split at %d: PromptSeq = %d (has=%v), want 0", cut, got.PromptSeq, got.HasPrompt)
		}
		if got.OutputSeq != wantOutput {
			t.Errorf("split at %d: OutputSeq = %d, want %d", cut, got.OutputSeq, wantOutput)
		}
		if got.Command != "ls" {
			t.Errorf("split at %d: Command = %q, want %q", cut, got.Command, "ls")
		}
	}
}

// A byte-by-byte feed is the worst case for position tracking and must still be exact.
func TestBoundaryTrackerHandlesOneByteAtATime(t *testing.T) {
	full := promptSeq() + "$ x\n" + cmdSeq("x") + "out\n"
	tr := NewBoundaryTracker(0)
	for i := range len(full) {
		tr.Feed([]byte(full[i : i+1]))
	}

	want := []Boundary{{
		PromptSeq: 0,
		HasPrompt: true,
		OutputSeq: uint64(len(promptSeq()) + len("$ x\n") + len(cmdSeq("x"))),
		HasOutput: true,
		Command:   "x",
	}}
	if got := tr.Blocks(); !reflect.DeepEqual(got, want) {
		t.Errorf("Blocks() = %+v\nwant %+v", got, want)
	}
}

// Positions are offsets from where the stream was told it starts, not from zero.
//
// A session's log does not start at zero: the server continues the shim's numbering, so a tracker
// attached to an adopted session begins partway in. Without honoring that, every position would be an
// offset from the wrong origin and every read would land in the wrong place.
func TestBoundaryTrackerHonorsAStartingPosition(t *testing.T) {
	const start = 100_000

	tr := NewBoundaryTracker(0)
	tr.SetPosition(start)
	tr.Feed([]byte(promptSeq() + "$ y\n" + cmdSeq("y") + "out\n"))

	blocks := tr.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].PromptSeq != start {
		t.Errorf("PromptSeq = %d, want %d", blocks[0].PromptSeq, start)
	}
	wantOutput := uint64(start + len(promptSeq()) + len("$ y\n") + len(cmdSeq("y")))
	if blocks[0].OutputSeq != wantOutput {
		t.Errorf("OutputSeq = %d, want %d", blocks[0].OutputSeq, wantOutput)
	}
}

// Several commands produce several blocks, in order.
func TestBoundaryTrackerRecordsSeveralBlocks(t *testing.T) {
	tr := NewBoundaryTracker(0)
	for _, cmd := range []string{"one", "two", "three"} {
		tr.Feed([]byte(promptSeq() + "$ " + cmd + "\n" + cmdSeq(cmd) + "out\n" + doneSeq("0")))
	}

	blocks := tr.Blocks()
	if len(blocks) != 3 {
		t.Fatalf("got %d blocks, want 3: %+v", len(blocks), blocks)
	}
	for i, want := range []string{"one", "two", "three"} {
		if blocks[i].Command != want {
			t.Errorf("block %d command = %q, want %q", i, blocks[i].Command, want)
		}
	}
	// Strictly increasing, since each block starts after the previous one's output.
	for i := 1; i < len(blocks); i++ {
		if blocks[i].PromptSeq <= blocks[i-1].PromptSeq {
			t.Errorf("block %d starts at %d, not after block %d at %d",
				i, blocks[i].PromptSeq, i-1, blocks[i-1].PromptSeq)
		}
	}
}

// SinceCommands anchors at the prompt, so a multi-command read is self-delimiting.
//
// This is the whole point of the feature: consecutive command outputs with nothing between them cannot
// be told apart, so the prompt and the echoed command line are what separate one block from the next.
func TestSinceCommandsAnchorsAtThePrompt(t *testing.T) {
	tr := NewBoundaryTracker(0)
	var stream string
	starts := make(map[string]uint64)
	for _, cmd := range []string{"first", "second", "third"} {
		starts[cmd] = uint64(len(stream))
		stream += promptSeq() + "$ " + cmd + "\n" + cmdSeq(cmd) + "out-" + cmd + "\n" + doneSeq("0")
	}
	tr.Feed([]byte(stream))

	tests := []struct {
		n    int
		want uint64
	}{
		{n: 1, want: starts["third"]},
		{n: 2, want: starts["second"]},
		{n: 3, want: starts["first"]},
	}
	for _, tc := range tests {
		seq, available, ok := tr.SinceCommands(tc.n)
		if !ok {
			t.Errorf("SinceCommands(%d) not ok, available = %d", tc.n, available)
			continue
		}
		if seq != tc.want {
			t.Errorf("SinceCommands(%d) = %d, want %d", tc.n, seq, tc.want)
		}
	}
}

// Asking for more commands than were seen reports how many there are.
//
// So a caller can say "only 2 commands are known" rather than silently returning fewer than asked for,
// which would look like the session ran less than it did.
func TestSinceCommandsReportsWhatIsAvailable(t *testing.T) {
	tr := NewBoundaryTracker(0)
	for _, cmd := range []string{"a", "b"} {
		tr.Feed([]byte(promptSeq() + cmdSeq(cmd) + "out\n" + doneSeq("0")))
	}

	seq, available, ok := tr.SinceCommands(5)
	if ok {
		t.Errorf("SinceCommands(5) = (%d, ok) with only 2 commands, want not ok", seq)
	}
	if available != 2 {
		t.Errorf("available = %d, want 2", available)
	}
}

// A session that has reported nothing cannot answer, and says so.
//
// The case that matters in practice: a shell with no OSC 133 integration produces no boundaries at all,
// and this has to be distinguishable from a session that simply printed nothing.
func TestSinceCommandsWithNoBoundaries(t *testing.T) {
	tr := NewBoundaryTracker(0)
	tr.Feed([]byte("just some output with no markers at all\n"))

	if _, available, ok := tr.SinceCommands(1); ok || available != 0 {
		t.Errorf("SinceCommands(1) = (available=%d, ok=%v), want (0, false)", available, ok)
	}
	if _, ok := tr.LastOutput(); ok {
		t.Error("LastOutput() ok with no markers, want not ok")
	}
}

// A trailing prompt nobody has typed at must not consume one of the n.
//
// A shell sitting idle has drawn a prompt after its last command, so the newest block ran nothing.
// Counting it would make `--since-commands 1` return the empty tail rather than the last command, which
// is never what was meant.
func TestSinceCommandsSkipsATrailingPrompt(t *testing.T) {
	tr := NewBoundaryTracker(0)

	// The real command starts the stream, so its prompt is at position zero.
	const lastStart = 0
	stream := promptSeq() + cmdSeq("real") + "output\n" + doneSeq("0")
	// Then an idle prompt, as precmd emits after the command finishes.
	stream += promptSeq()
	tr.Feed([]byte(stream))

	if n := tr.Count(); n != 2 {
		t.Fatalf("got %d blocks, want 2 (the command and the idle prompt)", n)
	}
	seq, available, ok := tr.SinceCommands(1)
	if !ok {
		t.Fatalf("SinceCommands(1) not ok, available = %d", available)
	}
	if seq != lastStart {
		t.Errorf("SinceCommands(1) = %d, want %d: the idle prompt must not count", seq, lastStart)
	}
	if available != 1 {
		t.Errorf("available = %d, want 1: only one block ran a command", available)
	}
}

// LastOutput excludes the prompt and the echoed command, unlike SinceCommands(1).
func TestLastOutputExcludesThePrompt(t *testing.T) {
	tr := NewBoundaryTracker(0)
	stream := promptSeq() + "$ build\n" + cmdSeq("build") + "compiling\n"
	tr.Feed([]byte(stream))

	outSeq, ok := tr.LastOutput()
	if !ok {
		t.Fatal("LastOutput() not ok")
	}
	wantOut := uint64(len(promptSeq()) + len("$ build\n") + len(cmdSeq("build")))
	if outSeq != wantOut {
		t.Errorf("LastOutput() = %d, want %d", outSeq, wantOut)
	}

	// And it differs from the transcript form, which is the reason both exist.
	sinceSeq, _, ok := tr.SinceCommands(1)
	if !ok {
		t.Fatal("SinceCommands(1) not ok")
	}
	if sinceSeq >= outSeq {
		t.Errorf("SinceCommands(1) = %d and LastOutput() = %d, want the prompt anchor to come first",
			sinceSeq, outSeq)
	}
}

// A repeated command marker is one command, not two.
func TestBoundaryTrackerIgnoresRepeatedCommandMarkers(t *testing.T) {
	tr := NewBoundaryTracker(0)
	tr.Feed([]byte(promptSeq() + cmdSeq("once") + cmdSeq("once") + "out\n"))

	if n := tr.Count(); n != 1 {
		t.Errorf("got %d blocks for a repeated C marker, want 1: %+v", n, tr.Blocks())
	}
}

// A prompt arriving while a command is open closes it, so the next block is not merged into it.
//
// This is the interrupted-command case: a shell that is interrupted may draw a new prompt without ever
// reporting the old command's end, and CommandTracker already tolerates it for the same reason.
func TestBoundaryTrackerClosesABlockOnAPromptWithoutDone(t *testing.T) {
	tr := NewBoundaryTracker(0)
	// No D marker between them, as an interrupt produces.
	tr.Feed([]byte(promptSeq() + cmdSeq("interrupted") + "partial"))
	tr.Feed([]byte(promptSeq() + cmdSeq("next") + "out\n"))

	blocks := tr.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(blocks), blocks)
	}
	if blocks[0].Command != "interrupted" || blocks[1].Command != "next" {
		t.Errorf("commands = %q, %q, want interrupted, next", blocks[0].Command, blocks[1].Command)
	}
}

// A command with no preceding prompt still records a boundary.
//
// What a tracker attached mid-command sees: the session was already running something when cm started
// following it, so there is no prompt position to anchor at and the command's own start is the best
// available answer.
func TestBoundaryTrackerCommandWithoutAPrompt(t *testing.T) {
	tr := NewBoundaryTracker(0)
	tr.Feed([]byte(cmdSeq("already-running") + "output\n"))

	blocks := tr.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].HasPrompt {
		t.Error("HasPrompt = true with no prompt marker seen, want false")
	}
	if !blocks[0].HasOutput {
		t.Error("HasOutput = false, want the command recorded")
	}
	// SinceCommands falls back to the command's own start rather than refusing.
	seq, _, ok := tr.SinceCommands(1)
	if !ok {
		t.Fatal("SinceCommands(1) not ok")
	}
	if seq != blocks[0].OutputSeq {
		t.Errorf("SinceCommands(1) = %d, want the output position %d", seq, blocks[0].OutputSeq)
	}
}

// History is bounded, and the oldest boundaries are dropped first.
//
// A long-lived shell runs commands indefinitely, so this grows without a bound. The newest are what a
// caller reads back, so those are what is kept.
func TestBoundaryTrackerBoundsHistory(t *testing.T) {
	const max = 4
	tr := NewBoundaryTracker(max)
	for _, cmd := range []string{"c1", "c2", "c3", "c4", "c5", "c6"} {
		tr.Feed([]byte(promptSeq() + cmdSeq(cmd) + "out\n" + doneSeq("0")))
	}

	blocks := tr.Blocks()
	if len(blocks) != max {
		t.Fatalf("got %d blocks, want %d", len(blocks), max)
	}
	// The last four, in order.
	want := []string{"c3", "c4", "c5", "c6"}
	for i, w := range want {
		if blocks[i].Command != w {
			t.Errorf("block %d = %q, want %q (oldest must be dropped)", i, blocks[i].Command, w)
		}
	}
	// And asking beyond what is retained reports the bound rather than claiming more.
	if _, available, ok := tr.SinceCommands(max + 1); ok || available != max {
		t.Errorf("SinceCommands(%d) = (available=%d, ok=%v), want (%d, false)",
			max+1, available, ok, max)
	}
}

// A command line containing a semicolon survives, since the escaping exists for that.
func TestBoundaryTrackerKeepsEscapedSemicolons(t *testing.T) {
	tr := NewBoundaryTracker(0)
	tr.Feed([]byte(promptSeq() + "\x1b]133;C;cmdline=echo a\\;b\x07" + "out\n"))

	blocks := tr.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].Command != "echo a;b" {
		t.Errorf("Command = %q, want %q", blocks[0].Command, "echo a;b")
	}
}

// Bytes with no markers still advance the position.
//
// The early-return path for ordinary output is where this is easy to break: skipping the position update
// makes every later boundary too low by however much plain output went past.
func TestBoundaryTrackerAdvancesPositionOverPlainOutput(t *testing.T) {
	tr := NewBoundaryTracker(0)
	filler := strings.Repeat("x", 5000)
	tr.Feed([]byte(filler))
	tr.Feed([]byte(promptSeq() + cmdSeq("after") + "out\n"))

	blocks := tr.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1", len(blocks))
	}
	if blocks[0].PromptSeq != uint64(len(filler)) {
		t.Errorf("PromptSeq = %d, want %d: plain output must advance the position",
			blocks[0].PromptSeq, len(filler))
	}
}

// Both terminators are legal and shells use both.
func TestBoundaryTrackerAcceptsStringTerminator(t *testing.T) {
	tr := NewBoundaryTracker(0)
	// ESC-backslash rather than BEL.
	tr.Feed([]byte("\x1b]133;A\x1b\\" + "\x1b]133;C;cmdline=st\x1b\\" + "out\n"))

	blocks := tr.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(blocks), blocks)
	}
	if blocks[0].Command != "st" {
		t.Errorf("Command = %q, want %q", blocks[0].Command, "st")
	}
	wantOut := uint64(len("\x1b]133;A\x1b\\") + len("\x1b]133;C;cmdline=st\x1b\\"))
	if blocks[0].OutputSeq != wantOut {
		t.Errorf("OutputSeq = %d, want %d", blocks[0].OutputSeq, wantOut)
	}
}

// SetPosition discards held-back bytes, since they belonged to the previous stream.
func TestBoundaryTrackerSetPositionDropsPartial(t *testing.T) {
	tr := NewBoundaryTracker(0)
	// A fragment that could begin a marker.
	tr.Feed([]byte("output\x1b]133"))
	// Repositioned, as happens when a session is re-adopted at a new offset.
	tr.SetPosition(500)
	tr.Feed([]byte(";A\x07" + cmdSeq("fresh") + "out\n"))

	// The stale fragment must not have been spliced onto the new stream. Whether the truncated ";A"
	// registers is not the point; the position must be from the new origin.
	for _, b := range tr.Blocks() {
		if b.HasPrompt && b.PromptSeq < 500 {
			t.Errorf("PromptSeq = %d, want >= 500: a stale fragment was spliced in", b.PromptSeq)
		}
		if b.HasOutput && b.OutputSeq < 500 {
			t.Errorf("OutputSeq = %d, want >= 500", b.OutputSeq)
		}
	}
}
