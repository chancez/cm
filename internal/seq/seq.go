// Package seq names cm's two sequence-number spaces so the compiler can tell them apart.
//
// There are two because output is rewritten on the way through. The shim numbers the bytes it read
// from the pty. The server then rewrites some of them -- `RewritePromptRedraw` appends nine bytes to a
// prompt marker that carries no `redraw` parameter, and the graphics scan removes commands it consumed
// -- and numbers what clients actually receive. The two therefore drift apart over a session's life,
// by an amount nothing tracks and nobody can guess.
//
// Both were `uint64`, which is why mixing them compiled. That cost three bugs, all silent:
//
//   - Adoption stored one number and used it for both. The adopting server started its client log at
//     the shim's position while a reconnecting client resumed from a position counted in post-rewrite
//     bytes, and `Subscribe` clamps a position past the end to the end, so the difference was skipped
//     without a word. Nine bytes per prompt marker, 27 across three commands: enough to eat the front
//     of an escape sequence and leave the remainder rendering as literal text.
//   - `cm read --since-commands` took boundaries from the pre-rewrite stream and read them out of the
//     post-rewrite log, so the anchor drifted by nine bytes per prompt and the drift grew all session.
//   - `modelSeq` had to be documented as living in the log's numbering rather than the shim's, because
//     nothing in the type said so.
//
// Every one presented as corrupted output a long way from its cause, and none of them failed loudly.
// A distinct type per space is the cheapest thing that makes the mistake impossible rather than
// merely documented: `Subscribe(lastSeq)` no longer builds.
package seq

// Number is the constraint for a sequence-number space, so seqlog can be written once and instantiated
// per space.
type Number interface {
	~uint64
}

// Shim counts bytes as the shim numbered them, before the server rewrote anything.
//
// This is the space a resubscribe names, because it is the shim's own log being asked, and the shim
// knows nothing about the rewrite.
type Shim uint64

// Log counts bytes as the server's output log numbers them, after the rewrite.
//
// This is the space clients live in: what a client received, where it resumes from, and the position
// the terminal model's screen corresponds to.
type Log uint64
