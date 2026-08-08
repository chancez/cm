package osc

import "bytes"

// RewritePromptRedraw forces redraw=0 into OSC 133;A prompt markers.
//
// OSC 133 lets a shell tell the terminal where its prompt starts, and redraw=1 means "I will
// repaint this prompt myself if you need me to". A terminal that believes this clears the prompt
// lines on resize and waits for the shell to redraw them.
//
// That is fine when the shell talks to the terminal directly. Through a multiplexer it is not:
// the redraw the shell performs goes to its pty, in that pty's coordinates, and by the time it
// reaches the outer terminal the cursor positions no longer mean the same thing. The result is a
// prompt that has been cleared and never comes back. Forcing redraw=0 keeps the outer terminal
// from clearing anything it cannot get repainted. zmx does the same, for the same reason.
//
// The input is returned unchanged when it contains no prompt markers, which is the overwhelmingly
// common case, so this is cheap to apply to every chunk of output.
func RewritePromptRedraw(p []byte) []byte {
	if !bytes.Contains(p, []byte(promptStart)) {
		return p
	}

	var out []byte
	rest := p
	for {
		i := bytes.Index(rest, []byte(promptStart))
		if i < 0 {
			out = append(out, rest...)
			return out
		}

		// Find where this OSC ends. Either terminator is legal, and shells use both.
		seqStart := i
		tail := rest[seqStart:]
		end, termLen := oscEnd(tail)
		if end < 0 {
			// An unterminated sequence means the rest of the chunk is a partial escape. Passing
			// it through unchanged is right: rewriting a fragment would corrupt it, and the
			// remainder arrives in the next chunk.
			out = append(out, rest...)
			return out
		}

		seq := tail[:end]
		out = append(out, rest[:seqStart]...)
		out = append(out, rewriteOne(seq)...)
		out = append(out, tail[end:end+termLen]...)
		rest = tail[end+termLen:]
	}
}

// promptStart is the OSC 133 prompt-start introducer.
const promptStart = "\x1b]133;A"

// oscEnd locates the terminator of an OSC sequence, returning its offset and length.
//
// Returns -1 when the sequence is not terminated within p.
func oscEnd(p []byte) (int, int) {
	if i := bytes.IndexByte(p, 0x07); i >= 0 {
		// BEL terminator.
		if j := bytes.Index(p, []byte("\x1b\\")); j >= 0 && j < i {
			return j, 2
		}
		return i, 1
	}
	if j := bytes.Index(p, []byte("\x1b\\")); j >= 0 {
		// ST terminator.
		return j, 2
	}
	return -1, 0
}

// rewriteOne sets redraw=0 on a single OSC 133;A sequence.
func rewriteOne(seq []byte) []byte {
	// Already correct, so leave the bytes exactly as they are.
	if bytes.Contains(seq, []byte("redraw=0")) {
		return seq
	}
	if i := bytes.Index(seq, []byte("redraw=1")); i >= 0 {
		out := make([]byte, len(seq))
		copy(out, seq)
		// Same length, so an in-place substitution cannot disturb anything else in the sequence.
		copy(out[i:], []byte("redraw=0"))
		return out
	}
	// No redraw parameter at all. Appending one is what makes this reliable: the default is
	// terminal-defined, so leaving it unset means depending on the client's choice.
	return append(append([]byte{}, seq...), []byte(";redraw=0")...)
}
