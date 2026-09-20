package client

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/chancez/cm/internal/osc"
)

// nestingAnnouncer tells whatever owns this client's stdout that a cm client is attached here.
//
// It exists for one case: a `cm attach` on the far side of an ssh. `CM_SESSION` does not cross ssh, so
// there is no parent to name in Open, the server it talks to has no idea it is nested, and the outer
// window goes on intercepting the detach key -- which for a per-window session closes the window instead
// of leaving the remote session. The announcement travels the only channel that does reach the parent,
// which is this client's own output.
//
// Nothing else changes when it is heard: it is a fact published to the parent's clients, the same as an
// attachment the server was told about. See the Hosting message in the proto for what a parent does with
// it, and osc.Nesting for the wire form.
type nestingAnnouncer struct {
	// id is this client's nonce, which pairs the withdrawal with the announcement.
	//
	// Random rather than the pid, which was the first choice and is wrong: an ssh chain announces once per
	// hop and every hop's announcement passes through the outermost pty, so two hosts with the same pid
	// would look like one client to the parent that hears both.
	id string
	// session is what this client attached to, for a reader of the parent's location rather than for cm.
	//
	// Starts as the reference the caller gave and is refined to the session's own name once the server
	// answers, which is not a nicety: `cm tui` attaches by ID on purpose, so without the refinement the
	// flow this exists for reports "@a7k2m9x4" where a person wanted "books". See refine.
	session string
	// out is where the announcement goes: the one writer for this client's terminal, so an announcement
	// cannot land inside a half-written sequence.
	out *screen
}

// newNestingAnnouncer returns an announcer, or nil when this client has nothing to announce or nowhere to
// announce it.
//
// Two conditions, and both are about whether the announcement would mean anything.
//
// A client that named a parent in Open needs none of this: its own server already knows, by the route that
// also guarantees a withdrawal when the stream ends. Announcing as well would have the parent count one
// client twice.
//
// A client that reads no keys has no detach key to be handed, so there is nothing for a parent to hand
// over. That also keeps the sequence out of a stream where it would be corruption rather than
// information: `cm read --follow` writes to a pipe, and a file with an OSC in it is a broken file.
func newNestingAnnouncer(opts Options, tty *TTY, scr *screen) *nestingAnnouncer {
	if opts.InsideSession != "" {
		return nil
	}
	if !opts.readsTerminal(tty) || !opts.overlayEnabled(tty) {
		return nil
	}
	return &nestingAnnouncer{id: newNestingID(), session: opts.Session, out: scr}
}

// refine replaces the announced session with the name the server gave, and re-announces if it changed.
//
// Called when an Open answers, which is the first moment this client knows what the session is called
// rather than how it was asked for. Worth a second sequence because the reference a client attaches with is
// often not a name: `cm tui` uses the ID deliberately, and the ID is exactly what a person reading a
// location cannot use. A switch reaches here again through the next Open, which is what keeps the parent's
// view following a client that moved.
//
// Silent when nothing changed, which is the ordinary attach by name.
func (n *nestingAnnouncer) refine(session string) {
	if n == nil || session == "" || session == n.session {
		return
	}
	n.session = session
	n.write(false)
}

// announce says this client is attached, and is called again on every reconnect.
//
// Repeated deliberately. The parent holds the announced state in memory only, so a parent whose server
// restarted has forgotten it while this client is still here, exactly as an RPC-known attachment re-sends
// inside_session when it reopens. The parent keys on the nonce and ignores a repeat, so the cost of saying
// it again is one sequence.
func (n *nestingAnnouncer) announce() {
	if n == nil {
		return
	}
	n.write(false)
}

// withdraw says this client is leaving, which is what gives the parent its keys back.
//
// Best effort by nature: a killed client and a dropped link both send nothing, and the parent is left
// believing a client is there. That is why an announced nesting does not take the overlay's prefix key,
// and why the parent bounds how many it tracks. The lasting fix is a collector on the parent's side, which
// wants the shell to mark its command frames; see docs/ideas.md on a session's location.
func (n *nestingAnnouncer) withdraw() {
	if n == nil {
		return
	}
	n.write(true)
}

func (n *nestingAnnouncer) write(ended bool) {
	// Errors are dropped rather than reported: a terminal that cannot be written to is already failing in
	// louder ways, and an announcement that does not arrive costs the detach key handover rather than the
	// attachment.
	_ = n.out.inject(osc.NestingSequence(n.id, n.session, ended))
}

// newNestingID draws a nonce for one client.
//
// Hex, which keeps it inside the character set the parser accepts. The fallback is reached only if the
// kernel's entropy source fails, where refusing to announce would be worse than a weaker nonce: the pid is
// unique among live processes on this host, and the host is what a collision would have to cross.
func newNestingID() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("p%d", os.Getpid())
	}
	return hex.EncodeToString(buf[:])
}
