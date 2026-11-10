package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/vt"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// TestAttachReplaysASequenceTheModelHasNotFinished is the other half of the model-lag hazard next door.
//
// TestAttachStreamsFromTheModelNotTheLog covers output the model has not reached: the log is ahead, so
// streaming from the log's end would skip it. This covers output the model has *consumed* but cannot
// represent. A partial escape sequence lives in the emulator's parser rather than on its screen, and
// Restore serializes the screen, so resuming at the model's position puts those bytes in neither the
// snapshot nor the stream and no client ever sees them.
//
// The symptom: the front of a sequence disappears and its tail renders as text. A program wrote
// `ESC ] 2;fidelity BEL ESC [ 38:2:1` and paused, and an attaching client got the title set and `:2:3m`
// printed on screen, with the nine bytes that opened the SGR gone.
//
// Which bytes are unfinished is libghostty's answer, pinned against the real parser in
// internal/vt.TestContinuation. What this pins is where the server puts them: at the very end of the
// restore, after the images, with the stream starting exactly where the model stopped.
//
// Constructed rather than driven through a pty: racing a real program onto "the model has consumed a
// sequence it cannot yet render" failed about one run in eight. Here it is exact.
func TestAttachReplaysASequenceTheModelHasNotFinished(t *testing.T) {
	const (
		complete = "\x1b]2;fidelity\x07"
		partial  = "\x1b[38:2:1"
		// Output the pump delivers after the attach, so the stream read ends without waiting out a
		// timeout to prove an absence.
		after = "\x1b[0mNEXT"
		// The program status records the restore carries, here none. See
		// TestAttachRestoresProgramStatusRecords.
		noRecords = "\x1b]7501;state=clear\a"
	)
	placements := []graphics.Placement{{ImageID: 7, Row: 1, Col: 2, Columns: 3, Rows: 2}}

	tests := []struct {
		name            string
		continuation    []byte
		continuationErr error
		// images attaches a client that draws them, so the restore carries placements too.
		images      bool
		wantRestore string
	}{
		{
			name:         "unfinished sequence",
			continuation: []byte(partial),
			wantRestore:  noRecords + "SCREEN" + partial,
		},
		{
			// Last, after the placements: the stream completes the sequence, so anything between the two
			// would land inside it.
			name:         "after the images",
			continuation: []byte(partial),
			images:       true,
			wantRestore:  noRecords + "SCREEN" + string(graphics.PlaceCommands(placements, 0)) + partial,
		},
		{
			name:        "between sequences",
			wantRestore: noRecords + "SCREEN",
		},
		{
			// A sequence past vt.MaxContinuation is not tracked. The attach still succeeds with the screen.
			name:            "unreadable",
			continuationErr: errors.New("reading continuation: invalid value"),
			wantRestore:     noRecords + "SCREEN",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &Session{
				id:     "modelpartial",
				log:    cmlog.Discard(),
				recent: seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
				term: &fakeTerminal{
					restore:         []byte("SCREEN"),
					continuation:    tt.continuation,
					continuationErr: tt.continuationErr,
					placements:      placements,
				},
				clientSizes: make(map[*attachToken]*clientSize),
				evicts:      make(map[*attachToken]chan struct{}),
				metaSubs:    make(map[*metaSub]struct{}),
				boundaries:  osc.NewBoundaryTracker(0),
				done:        make(chan struct{}),
			}

			// Delivered and then fed, which is the order the pump uses, through feedTerminal so modelSeq is
			// set the way the pump sets it.
			sess.recent.Append([]byte(complete + partial))
			sess.feedTerminal([]byte(complete+partial), sess.recent.Next())

			var tok *attachToken
			if tt.images {
				tok = drawingClient(t, sess)
			}
			att, err := sess.attach(nil, tok)
			if err != nil {
				t.Fatalf("attach() error = %v", err)
			}
			defer sess.detach(att)

			if got := string(att.restore); got != tt.wantRestore {
				t.Errorf("restore = %q, want %q", got, tt.wantRestore)
			}
			// Followers are sent this in place of the restore. See Service.Attach.
			if got, want := string(att.pending), string(tt.continuation); tt.continuationErr == nil && got != want {
				t.Errorf("pending = %q, want %q", got, want)
			}

			// The stream picks up where the model stopped. Not before, which would send the unfinished
			// sequence twice, once in the restore and once streamed, and the completed one with it.
			sess.recent.Append([]byte(after))
			if got := readUntil(t, att.reader, "NEXT"); got != after {
				t.Errorf("attached client streamed %q, want %q", got, after)
			}
		})
	}
}

// TestFollowerReceivesTheSequenceItJoinsPartway is the same hazard for a client that takes no restore.
//
// A follower (`cm read --follow`) drops the screen, since it streams what happens next, but it starts at
// the same position a painting client does: where the model stopped. When that is inside a sequence, the
// stream begins with the sequence's tail, which a raw follower writes out as a broken sequence and a
// plain one prints as text. So a follower is sent the unfinished sequence in place of the restore.
//
// Through the real shim, model and service rather than a fake: what matters is that the bytes reach a
// follower's stream, and the unfinished state has to be one the real model holds. An OSC 52 rather than a
// short CSI because the pump holds back a short unfinished tail itself (maxHeldTail), so only a long one
// ever reaches the model unfinished.
func TestFollowerReceivesTheSequenceItJoinsPartway(t *testing.T) {
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return vt.NewSessionTerminal(rows, cols, 0)
	})
	ctx := context.Background()

	opener := "\x1b]52;c;"
	payload := strings.Repeat("A", 5000)
	rec := startShimFor(t, shimConfigFor("follow-partial",
		`printf '\033]52;c;'; head -c 5000 /dev/zero | tr '\0' A; sleep 5`))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Waited for in the model, not the log. A follower attaching before the model consumed the bytes
	// streams them from the log and passes whether or not the fix is there.
	sess, _ := mgr.Get("follow-partial")
	deadline := time.Now().Add(5 * time.Second)
	for {
		sess.termMu.Lock()
		pending, _ := sess.term.Continuation()
		sess.termMu.Unlock()
		if len(pending) == len(opener)+len(payload) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("model never held the whole unfinished OSC 52, last continuation was %d bytes", len(pending))
		}
		time.Sleep(10 * time.Millisecond)
	}

	svc := NewService(mgr)
	streamCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stream := newHeldFakeStream(streamCtx,
		openReq(&serverv1.Open{Session: "follow-partial", Rows: 24, Cols: 80, ReadOnly: true, NoRestore: true}),
	)
	attachDone := make(chan struct{})
	go func() {
		defer close(attachDone)
		_ = svc.Attach(streamCtx, stream)
	}()

	want := opener + payload
	var got string
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got = followerOutput(stream); len(got) >= len(want) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stream.closeStream()
	<-attachDone

	if got != want {
		t.Errorf("follower received %q, want %q: it joined partway through the OSC 52 and was not sent "+
			"its start", trunc(got), trunc(want))
	}
}

// trunc shortens a long string for a failure message, keeping both ends since either can be the difference.
func trunc(s string) string {
	if len(s) <= 80 {
		return s
	}
	return s[:40] + "..." + s[len(s)-40:]
}
