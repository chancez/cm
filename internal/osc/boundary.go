package osc

import "bytes"

// DefaultBoundaryHistory is how many command boundaries a tracker retains.
//
// Bounded because this grows with every command a long-lived shell runs, and a session can live for
// days. 64 is chosen to be more than anyone reads back interactively -- `cm read --since-commands 64`
// is already far past what fits on a screen -- while costing a couple of hundred bytes per session.
//
// The output itself is bounded separately and more tightly, so a boundary usually outlives the bytes it
// points at. That asymmetry is deliberate: a boundary whose output has been trimmed still tells a
// caller the command existed, which is a better answer than claiming it never ran.
const DefaultBoundaryHistory = 64

// Boundary marks where one command's block begins in a stream of output.
//
// Two positions rather than one, because "the output of a command" has two defensible meanings and
// callers want different ones. PromptSeq is where the shell started drawing its prompt, so reading from
// it yields the prompt, the echoed command line, and the output -- a transcript that says what ran.
// OutputSeq is where the command itself started, so reading from it yields only what the program
// printed, which is what a parser wants.
//
// A block with no output yet has OutputSeq set and nothing after it, which is normal for a command
// still running.
type Boundary struct {
	// PromptSeq is the position of the 133;A prompt marker that opened this block.
	//
	// Zero when the block was opened by a command start with no preceding prompt marker, which happens
	// when a tracker is attached mid-session and the first thing it sees is a command.
	PromptSeq uint64
	// HasPrompt distinguishes a genuinely unknown prompt position from position zero, which is a real
	// position at the very start of a session.
	HasPrompt bool
	// OutputSeq is the position just after the 133;C marker, where the command's own output begins.
	OutputSeq uint64
	// HasOutput reports whether a command actually started in this block. False for a prompt that has
	// been drawn but not yet used, which is the state a session sits in whenever nobody is typing.
	HasOutput bool
	// Command is the command line the shell reported, empty when it reported none.
	Command string
}

// BoundaryTracker records where each command's output begins in a sequenced stream.
//
// Separate from CommandTracker, which answers "what is happening now" and deliberately keeps no
// history. This answers "where did that start", which needs positions and therefore has to be fed the
// same bytes the log numbers.
//
// That last point is the whole reason this is its own type rather than fields on CommandTracker.
// CommandTracker is fed the shell's output *before* cm rewrites prompt markers, so it reads the markers
// exactly as the shell sent them; the log numbers bytes *after* the rewrite, and the rewrite can append
// nine bytes to a prompt marker that carried no redraw parameter. A position taken from the pre-rewrite
// stream would therefore drift from the log by nine bytes per prompt, silently, in a direction that
// grows over a session's life. See docs/architecture.md on the two sequence-number spaces.
//
// Not safe for concurrent use. In cm one output pump feeds it, which is also the only writer to
// terminal state.
type BoundaryTracker struct {
	// max bounds how many boundaries are kept. Zero means DefaultBoundaryHistory.
	max int
	// blocks holds the retained boundaries, oldest first.
	blocks []Boundary
	// pos is the sequence number of the next byte to be fed.
	pos uint64
	// partial holds a trailing fragment that may be the start of a sequence, along with the position it
	// began at, so a sequence split across two feeds is still located correctly.
	partial    []byte
	partialPos uint64
	// running reports whether a command is currently open, so a repeated 133;C does not open a second
	// block. Mirrors CommandTracker's transition counting for the same reason.
	running bool
}

// NewBoundaryTracker returns a tracker retaining max boundaries, or DefaultBoundaryHistory when max is
// zero or negative.
func NewBoundaryTracker(max int) *BoundaryTracker {
	if max <= 0 {
		max = DefaultBoundaryHistory
	}
	return &BoundaryTracker{max: max}
}

// SetPosition tells the tracker where the stream it is about to be fed begins.
//
// Needed because a session's log does not start at zero: the server continues the shim's numbering, and
// a tracker attached to an adopted session starts partway in. Without this every recorded position
// would be an offset from the wrong origin.
func (t *BoundaryTracker) SetPosition(seq uint64) {
	t.pos = seq
	// Held-back bytes belonged to the old stream, so keeping them would splice two positions together.
	t.partial = nil
	t.partialPos = 0
}

// Feed consumes a chunk of output and records any command boundaries it contains.
//
// The chunk must be the bytes as the log numbers them, and must be fed exactly once, in order.
func (t *BoundaryTracker) Feed(p []byte) {
	start := t.pos
	t.pos += uint64(len(p))

	buf := p
	// base is the sequence number of buf[0].
	base := start
	if len(t.partial) > 0 {
		buf = append(t.partial, p...)
		base = t.partialPos
		t.partial = nil
	}

	// The common case: no marker and no possible start of one, so nothing to record. Checked after the
	// position is advanced, since the bytes still count toward it.
	if !bytes.Contains(buf, []byte(commandIntro)) && partialPrefixLen(buf) == 0 {
		return
	}

	for {
		i := bytes.Index(buf, []byte(commandIntro))
		if i < 0 {
			t.holdBack(buf, base)
			return
		}

		tail := buf[i:]
		end, termLen := oscEnd(tail)
		if end < 0 {
			// Unterminated: the parameters are still arriving, so hold the fragment with its position.
			t.holdBack(tail, base+uint64(i))
			return
		}

		params := tail[len(commandIntro):end]
		// seqStart is where this marker begins, and seqEnd is just past its terminator, which is where
		// the bytes following it begin.
		seqStart := base + uint64(i)
		seqEnd := seqStart + uint64(end+termLen)
		t.apply(params, seqStart, seqEnd)

		consumed := i + end + termLen
		buf = buf[consumed:]
		base += uint64(consumed)
	}
}

// apply records one marker's effect on the boundary history.
func (t *BoundaryTracker) apply(params []byte, seqStart, seqEnd uint64) {
	if len(params) == 0 {
		return
	}

	switch params[0] {
	case 'A':
		// A new prompt opens a block. Only A and not B: both mean the shell is at a prompt, but they
		// bracket the prompt itself, so anchoring at B would exclude the prompt text that makes a
		// transcript readable.
		//
		// A prompt arriving while a command is open means the command ended without reporting D, which
		// happens when a shell is interrupted. Closing it here keeps the next block from being merged
		// into it.
		t.running = false
		t.push(Boundary{PromptSeq: seqStart, HasPrompt: true})

	case 'C':
		if t.running {
			// A repeated marker, not a second command. Matches how CommandTracker counts.
			return
		}
		t.running = true

		cmd := commandLine(params)
		// Attach to the open prompt block when there is one, so a block carries both positions. A
		// command with no preceding prompt opens its own block, which is what a tracker attached
		// mid-command sees.
		if n := len(t.blocks); n > 0 && t.blocks[n-1].HasPrompt && !t.blocks[n-1].HasOutput {
			t.blocks[n-1].OutputSeq = seqEnd
			t.blocks[n-1].HasOutput = true
			t.blocks[n-1].Command = cmd
			return
		}
		t.push(Boundary{OutputSeq: seqEnd, HasOutput: true, Command: cmd})

	case 'D':
		t.running = false
	}
}

// push appends a boundary, discarding the oldest once the bound is reached.
func (t *BoundaryTracker) push(b Boundary) {
	if len(t.blocks) == t.max {
		// Shifted rather than grown. A ring buffer would avoid the copy, and at 64 entries of two words
		// each the copy is not worth the index arithmetic that reading them back in order would need.
		copy(t.blocks, t.blocks[1:])
		t.blocks[len(t.blocks)-1] = b
		return
	}
	t.blocks = append(t.blocks, b)
}

// holdBack retains the part of buf that could be the start of a sequence, with its position.
func (t *BoundaryTracker) holdBack(buf []byte, base uint64) {
	keep := partialPrefixLen(buf)
	if keep == 0 {
		return
	}
	if keep > maxPartial {
		// Longer than any real sequence, so it is not one. See CommandTracker.holdBack.
		return
	}
	t.partial = append([]byte(nil), buf[len(buf)-keep:]...)
	t.partialPos = base + uint64(len(buf)-keep)
}

// Blocks returns the retained boundaries, oldest first.
func (t *BoundaryTracker) Blocks() []Boundary {
	out := make([]Boundary, len(t.blocks))
	copy(out, t.blocks)
	return out
}

// Count reports how many boundaries are retained.
func (t *BoundaryTracker) Count() int { return len(t.blocks) }

// SinceCommands returns the position where the last n command blocks begin.
//
// Counts blocks that actually ran a command, so a trailing prompt nobody has typed at does not consume
// one of the n. Without that, `--since-commands 1` at an idle shell would return the empty block after
// the last command rather than the command itself, which is never what was meant.
//
// Anchored at the prompt, so the output includes the prompt and the echoed command line. That is what
// makes a multi-command read parseable: consecutive outputs with nothing between them cannot be told
// apart, which is the problem this exists to solve.
//
// Reports how many blocks were available when it cannot satisfy the request, so a caller can say so
// rather than silently returning less than was asked for.
func (t *BoundaryTracker) SinceCommands(n int) (seq uint64, available int, ok bool) {
	if n <= 0 {
		return 0, 0, false
	}

	// Walk backwards collecting blocks that ran something.
	var ran []int
	for i := len(t.blocks) - 1; i >= 0; i-- {
		if t.blocks[i].HasOutput {
			ran = append(ran, i)
			if len(ran) == n {
				break
			}
		}
	}
	if len(ran) < n {
		return 0, len(ran), false
	}

	b := t.blocks[ran[len(ran)-1]]
	if b.HasPrompt {
		return b.PromptSeq, len(ran), true
	}
	// No prompt recorded for this block, which happens when the tracker attached mid-command. The
	// command's own start is the best available answer and is still a real boundary.
	return b.OutputSeq, len(ran), true
}

// LastOutput returns the position where the most recent command's own output begins.
//
// Distinct from SinceCommands(1), which includes the prompt and the echoed command. This is the form a
// parser wants: only what the program printed, with nothing of the shell around it. Well defined for one
// command only, which is why it is separate rather than a mode of the other.
func (t *BoundaryTracker) LastOutput() (seq uint64, ok bool) {
	for i := len(t.blocks) - 1; i >= 0; i-- {
		if t.blocks[i].HasOutput {
			return t.blocks[i].OutputSeq, true
		}
	}
	return 0, false
}
