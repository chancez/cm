package server

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/osc"
)

// frame returns the bytes a shell writes to open or close a command frame.
func frame(id, argv string, ended bool) []byte { return osc.FrameSequence(id, argv, ended) }

// The command a session is inside is what its shell says it is, in order.
func TestFramesBecomeTheSessionsLocation(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	if got := sess.Location(); got != nil {
		t.Errorf("Location() = %+v on a fresh session, want nil", got)
	}

	sess.processChunk(frame("local-1", "kitten ssh white", false), 0)
	want := []LocationFrame{{ID: "local-1", Argv: "kitten ssh white"}}
	if got := sess.Location(); !reflect.DeepEqual(got, want) {
		t.Errorf("Location() = %+v, want %+v", got, want)
	}

	// The far side's shell writes its own frames through the same pty once an ssh is involved, and they
	// stack above rather than replacing: that is what makes this a location rather than a single value.
	sess.processChunk(frame("remote-1", "nvim notes.md", false), 0)
	want = []LocationFrame{
		{ID: "local-1", Argv: "kitten ssh white"},
		{ID: "remote-1", Argv: "nvim notes.md"},
	}
	if got := sess.Location(); !reflect.DeepEqual(got, want) {
		t.Errorf("Location() with two hops = %+v, want %+v", got, want)
	}

	sess.processChunk(frame("remote-1", "", true), 0)
	want = []LocationFrame{{ID: "local-1", Argv: "kitten ssh white"}}
	if got := sess.Location(); !reflect.DeepEqual(got, want) {
		t.Errorf("Location() after the inner command returned = %+v, want %+v", got, want)
	}

	sess.processChunk(frame("local-1", "", true), 0)
	if got := sess.Location(); got != nil {
		t.Errorf("Location() back at the prompt = %+v, want nil", got)
	}
}

// A frame closing takes everything above it, which is what needs no timeout.
//
// The case: the ssh dies, so the remote shell never closes its own frames, but the local shell survives and
// its prompt hook closes the frame the ssh ran in. Without discarding descendants the stack would keep
// frames from a shell that no longer exists.
func TestClosingAFrameDiscardsTheOnesAboveIt(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("local-1", "ssh white", false), 0)
	sess.processChunk(frame("remote-1", "bash", false), 0)
	sess.processChunk(frame("remote-2", "tail -f log", false), 0)
	if got := len(sess.Location()); got != 3 {
		t.Fatalf("Location() depth = %d, want 3", got)
	}

	sess.processChunk(frame("local-1", "", true), 0)
	if got := sess.Location(); got != nil {
		t.Errorf("Location() = %+v, want nil: closing the outer frame discards what was above it", got)
	}
}

// A close for a frame nobody opened does nothing.
//
// Ordinary rather than defensive: two shells write to this pty once an ssh is involved, and the far side may
// have been running before cm was watching, so its first close names a frame this session never saw.
func TestAnUnmatchedFrameCloseIsIgnored(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("local-1", "ssh white", false), 0)
	sess.processChunk(frame("unknown-9", "", true), 0)

	want := []LocationFrame{{ID: "local-1", Argv: "ssh white"}}
	if got := sess.Location(); !reflect.DeepEqual(got, want) {
		t.Errorf("Location() = %+v, want %+v: an unmatched close must not disturb the stack", got, want)
	}
}

// The collector, which is what this whole mechanism is for.
//
// A client on the far side of an ssh announces itself so the outer window hands over its detach key, and a
// dropped link withdraws nothing. Binding the announcement to the command it was made in means the local
// shell reaching its next prompt is what discards it, with no timeout and no probing.
func TestClosingAFrameCollectsTheAnnouncementMadeInIt(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sess.processChunk(frame("local-1", "kitten ssh white cm attach books", false), 0)
	sess.processChunk(announce("remote-client", false), 0)

	want := hostingState{Nested: true, OnlyAnnounced: true}
	if got, ok := publishedHosting(sub); !ok || got != want {
		t.Fatalf("after the announcement, told (%+v, %v), want (%+v, true)", got, ok, want)
	}

	// The link dies: no withdrawal, and the local shell returns to its prompt.
	sess.processChunk(frame("local-1", "", true), 0)

	if got, ok := publishedHosting(sub); !ok || got != (hostingState{}) {
		t.Errorf("after the command returned, told (%+v, %v), want ({}, true): the announcement was not "+
			"collected, so this window would keep forwarding its detach key", got, ok)
	}
	if got := sess.AnnouncedClients(); got != 0 {
		t.Errorf("AnnouncedClients() = %d, want 0", got)
	}
}

// An announcement made with no frame open cannot be collected, and that is the documented limit rather than
// an oversight: a parent whose shell has no cm integration loaded has nothing to bind to.
func TestAnAnnouncementWithNoFrameIsNotCollected(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(announce("remote-client", false), 0)
	sess.processChunk(frame("local-1", "ssh white", false), 0)
	sess.processChunk(frame("local-1", "", true), 0)

	if got := sess.AnnouncedClients(); got != 1 {
		t.Errorf("AnnouncedClients() = %d, want 1: an announcement bound to no frame outlives every frame",
			got)
	}
}

// A client that withdraws normally is gone whether or not its frame closes, and the frame closing afterwards
// must not fire the collector on an announcement that is already gone.
func TestCollectingIsIdempotentWithAWithdrawal(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sess.processChunk(frame("local-1", "ssh white", false), 0)
	sess.processChunk(announce("remote-client", false), 0)
	if _, ok := publishedHosting(sub); !ok {
		t.Fatal("the announcement published nothing")
	}
	sess.processChunk(announce("remote-client", true), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{}) {
		t.Fatalf("after the withdrawal, told (%+v, %v), want ({}, true)", got, ok)
	}

	sess.processChunk(frame("local-1", "", true), 0)
	if got, ok := publishedHosting(sub); ok {
		t.Errorf("closing the frame published %+v, want nothing: the state had not changed", got)
	}
}

// An announcement made in a deeper frame goes when that frame's ancestor closes, which is the ssh chain.
func TestCollectingReachesAnnouncementsInDescendantFrames(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(frame("local-1", "ssh white", false), 0)
	sess.processChunk(frame("remote-1", "ssh black cm attach work", false), 0)
	sess.processChunk(announce("second-hop", false), 0)
	if got := sess.AnnouncedClients(); got != 1 {
		t.Fatalf("AnnouncedClients() = %d, want 1", got)
	}

	// The first hop's command returns, which takes the second hop's frame with it.
	sess.processChunk(frame("local-1", "", true), 0)
	if got := sess.AnnouncedClients(); got != 0 {
		t.Errorf("AnnouncedClients() = %d, want 0: a client announced two hops out was not collected", got)
	}
}

// Frames arrive as ordinary output, so anything that prints them is a source. Bounded, and bounded in the
// direction that keeps the outermost frames: those are the ones that say where the session went.
func TestFrameStackIsBounded(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	for i := 0; i < maxSessionFrames*3; i++ {
		sess.processChunk(frame(fmt.Sprintf("f%d", i), "ssh white", false), 0)
	}

	got := sess.Location()
	if len(got) != maxSessionFrames {
		t.Fatalf("Location() depth = %d, want %d", len(got), maxSessionFrames)
	}
	if got[0].ID != "f0" {
		t.Errorf("outermost frame = %q, want f0: the bound must drop the newest, not the oldest", got[0].ID)
	}
}

// A frame and an announcement in one chunk have to be applied in the order they arrived, or the
// announcement binds to nothing and is never collected.
func TestAFrameAndAnAnnouncementInOneChunkBind(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	chunk := append(frame("local-1", "ssh white cm attach books", false), announce("remote-client", false)...)
	sess.processChunk(chunk, 0)

	sess.processChunk(frame("local-1", "", true), 0)
	if got := sess.AnnouncedClients(); got != 0 {
		t.Errorf("AnnouncedClients() = %d, want 0: the announcement did not bind to the frame in its own chunk",
			got)
	}
}
