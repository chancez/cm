package server

import (
	"context"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/store"
)

// promptChunk is a chunk of shell output carrying an OSC 133;A prompt marker with no redraw
// parameter.
//
// That form is the one RewritePromptRedraw lengthens, by appending ";redraw=0", and it is what real
// shells send. A marker that already says redraw=1 is rewritten in place at the same length, so a
// fixture built from that form makes the two numbering spaces coincide and the bug below cannot
// appear at all.
const promptChunk = "\x1b]133;A\x07$ "

// The prompt rewrite changes length, which is what makes two numbering spaces exist in the first
// place.
//
// Asserted directly, and first, because every other test here rests on it. If a libghostty or shell
// change ever made the rewrite length-preserving, the drift would vanish and the tests below would
// pass for the wrong reason, so this states the premise rather than assuming it.
func TestPromptRewriteChangesLength(t *testing.T) {
	got := osc.RewritePromptRedraw([]byte(promptChunk))
	if len(got) == len(promptChunk) {
		t.Fatalf("RewritePromptRedraw(%q) = %q, same length.\n"+
			"The two sequence-number spaces only diverge because this rewrite lengthens output. If it "+
			"no longer does, the resume tests below are no longer testing anything.", promptChunk, got)
	}
	if want := len(promptChunk) + len(";redraw=0"); len(got) != want {
		t.Errorf("rewritten length = %d, want %d (%q -> %q)", len(got), want, promptChunk, got)
	}
}

// A session records where it served clients to, in the numbering clients see, not the shim's.
//
// This is the pair that has to be stored. LastSeq counts the shim's bytes because that is what a
// resubscribe asks for; ClientSeq counts what clients received. Conflating them is the bug, so the
// two are asserted to actually differ on a session that has produced prompt markers, rather than
// merely both being present.
func TestResumePointsUseSeparateNumberingSpaces(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("spaces", "printf '\\033]133;A\\007$ '; sleep 5"))
	sess, err := newSession(rec, &fakeTerminal{restore: []byte("R")}, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	sub := sess.recent.Subscribe(0)
	defer sub.Close()
	// Waiting for the rewritten form proves the rewrite happened before asserting on the positions it
	// shifts. Matching the pre-rewrite bytes would pass before the rewrite had run.
	readUntil(t, sub, "redraw=0")

	shimSeq, clientSeq := sess.resumePoints()
	if shimSeq == 0 || clientSeq == 0 {
		t.Fatalf("resumePoints() = (%d, %d), want both advanced past zero", shimSeq, clientSeq)
	}
	// Compared across the two spaces on purpose, which is what this test is for, so the conversion is
	// explicit. Everywhere else the types make this a compile error; see internal/seq.
	if uint64(clientSeq) <= uint64(shimSeq) {
		t.Errorf("resumePoints() = (shim %d, client %d), want the client position ahead.\n"+
			"The rewrite appends \";redraw=0\" to each prompt marker, so the bytes clients received are "+
			"longer than the bytes the shim sent. Equal values mean one number is being used for both "+
			"spaces, which is the defect.", shimSeq, clientSeq)
	}
}

// A client resuming after a server restart must be served the bytes it has not seen.
//
// This is the regression test for the bug. A client counts its resume position in the bytes it
// received, which are post-rewrite. The adopting server used to start its client log at LastSeq,
// which counts the shim's bytes, so the client asked for a position past the end of the new log.
// seqlog clamps a future position to the end, so those bytes were skipped silently, and an escape
// sequence straddling that point arrived with its front sliced off. It presented as a corrupted TUI
// that a forced repaint fixed.
//
// Driven through Manager.adopt with a record holding both positions, because that is the only place
// the two are turned into a log start, and asserted on what a resuming subscriber actually receives
// rather than on the numbers: the numbers looked plausible while the bytes were being dropped.
func TestAdoptedSessionServesAResumingClient(t *testing.T) {
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{rows: rows, cols: cols, restore: []byte("R")}, nil
	})
	ctx := context.Background()

	// A first server: run enough prompts that the two spaces are visibly apart, then record where it
	// had served clients to.
	rec := startShimFor(t, shimConfigFor("adopted",
		"printf '\\033]133;A\\007$ one\\r\\n\\033]133;A\\007$ two\\r\\n'; printf 'MARK\\n'; sleep 5"))
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	first, ok := mgr.Get("adopted")
	if !ok {
		t.Fatal("session was not adopted")
	}

	sub := first.recent.Subscribe(0)
	readUntil(t, sub, "MARK")
	// Where a client attached to the first server would have resumed from: the end of what it received.
	clientResume := sub.Position()
	sub.Close()

	shimSeq, clientSeq := first.resumePoints()
	if uint64(clientSeq) == uint64(shimSeq) {
		t.Fatalf("the two positions did not diverge (both %d), so this test would pass either way",
			shimSeq)
	}
	first.Close()

	// The second server adopts from the stored pair, which is what a restart does.
	rec.LastSeq, rec.ClientSeq = shimSeq, clientSeq
	second, err := mgr.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq, "")
	if err != nil {
		t.Fatalf("adopt() error = %v", err)
	}
	defer second.Close()

	// The client's position must be one the new log can serve. Before the fix the log began at
	// LastSeq, which is behind this, so Subscribe clamped forward and the client silently lost the
	// difference.
	if got := second.recent.Next(); got != clientResume {
		t.Errorf("adopted log begins at %d, want the client's resume position %d.\n"+
			"A log starting behind it makes the client ask for a position the log does not have, and "+
			"seqlog clamps a future position to the end, so the bytes between are dropped without a "+
			"word. That slices the front off whatever escape sequence spans the boundary.",
			got, clientResume)
	}

	// And a subscriber resuming there is served without being told its view is discontinuous, because
	// nothing was lost. Asserted through the chunk a read actually returns, since Gap is only
	// observable there, which is also the only place a client would ever see it.
	resumed := second.recent.Subscribe(clientResume)
	defer resumed.Close()
	if got := resumed.Position(); got != clientResume {
		t.Errorf("a subscriber asking for %d was positioned at %d, so its request was clamped",
			clientResume, got)
	}
}

// A record written before client_seq existed adopts as it used to, rather than from zero.
//
// The column defaults to 0, which is indistinguishable from a session that genuinely served nothing.
// Starting the log at zero would make every subscriber's position look far in the future, so the
// fallback is LastSeq: the old behavior for that one adoption, which is wrong by the rewrite drift
// but recovers on the next restart, rather than wrong by the whole session.
func TestAdoptFallsBackWhenClientSeqIsUnset(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("upgraded", "sleep 5"))
	rec.State = store.StateRunning
	rec.LastSeq = 4096
	rec.ClientSeq = 0 // an upgraded database

	sess, err := mgr.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq, "")
	if err != nil {
		t.Fatalf("adopt() error = %v", err)
	}
	defer sess.Close()

	if got := sess.recent.Next(); uint64(got) != uint64(rec.LastSeq) {
		t.Errorf("adopted log begins at %d, want the LastSeq fallback %d.\n"+
			"Zero in client_seq means the record predates the column. Taking it literally would start "+
			"the log at 0, so every client position would look like the distant future.",
			got, rec.LastSeq)
	}
}

// seqlog must report a gap when it clamps a position forward, rather than skipping bytes silently.
//
// Independent of the numbering fix and worth keeping either way. The clamp itself is legitimate: a log
// that was reset behind a subscriber should continue from the present. But the same branch also
// catches a position from a different numbering space, where bytes really are being skipped, and the
// two are indistinguishable from inside the log. Flagging both turns a silent hole into something a
// reader can act on.
func TestSubscribeFlagsAClampedPosition(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("clamped", "sleep 5"))
	sess, err := newSession(rec, nil, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	// Far past anything the log holds, which is the shape of a position counted in another space. The
	// flag is only visible on a delivered chunk, so output has to arrive before it can be asserted on.
	future := sess.recent.Subscribe(1 << 20)
	defer future.Close()
	sess.recent.Append([]byte("after"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chunk, err := future.Next(ctx)
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	if !chunk.Gap {
		t.Errorf("a subscriber positioned past the end of the log received %+v without Gap set.\n"+
			"Clamping it forward without saying so is how a resume across a server restart lost bytes "+
			"in the middle of an escape sequence and looked like a rendering bug.", chunk)
	}

	// A position the log can serve is not flagged, so the signal still discriminates.
	current := sess.recent.Subscribe(sess.recent.Next())
	defer current.Close()
	sess.recent.Append([]byte("more"))
	chunk, err = current.Next(ctx)
	if err != nil {
		t.Fatalf("Next() on a current subscriber error = %v", err)
	}
	if chunk.Gap {
		t.Errorf("a subscriber at the log's end received %+v with Gap set, want none", chunk)
	}
}
