package server

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
)

// announce returns the bytes a nested client writes to say it is there, naming no session.
//
// Most of these tests are about the handover rather than about the label, and a client too old to name its
// session sends exactly this, so it is also the compatibility case.
func announce(id string, ended bool) []byte { return osc.NestingSequence(id, "", ended) }

// announceSession is the same with the session the client attached to, which is what a current client sends.
func announceSession(id, session string) []byte { return osc.NestingSequence(id, session, false) }

// A client that cannot name its parent is heard through the output stream instead.
//
// The reported problem: `kitten ssh host cm attach work` inside a per-window session, where ctrl-\
// detached the *window* rather than the remote session. CM_SESSION does not cross ssh, so Open carries no
// inside_session, hostingParent finds nothing, and the parent never learns anything is nested.
//
// Driven through processChunk because that is the function the pump calls, so the trimming of a chunk that
// ends mid-sequence is exercised along with the tracker.
func TestAnnouncedNestingIsPublishedToTheParentsClients(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sess.processChunk(append([]byte("remote output"), announce("9f3c2a", false)...), 0)
	want := hostingState{Nested: true, OnlyAnnounced: true, Count: 1}
	if got, ok := publishedHosting(sub); !ok || got != want {
		t.Errorf("after a client announced itself, told (%+v, %v), want (%+v, true)", got, ok, want)
	}

	sess.processChunk(announce("9f3c2a", true), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{}) {
		t.Errorf("after it left, told (%+v, %v), want ({}, true)", got, ok)
	}
}

// An announced client is not a session this server has, so it must not be listed as one.
//
// `cm list` and `cm info` report hosting as session references a caller can act on. A nonce from another
// host is neither, and putting it there would make the field lie.
func TestAnnouncedNestingIsNotListedAsAHostedSession(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	sess.processChunk(announce("9f3c2a", false), 0)

	if got := sess.Hosting(); got != nil {
		t.Errorf("Hosting() = %v, want nil: an announced nonce is not a session on this server", got)
	}
}

// The same client announcing twice is one client, because that is what a reconnect looks like.
//
// The announced state lives only in memory here, so a client re-announces after a server restart the way
// the RPC path re-sends inside_session. Counting the repeat would leave the parent hosting forever once
// that client left.
func TestRepeatedAnnouncementIsTheSameClient(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sess.processChunk(announce("9f3c2a", false), 0)
	if _, ok := publishedHosting(sub); !ok {
		t.Fatal("the first announcement published nothing")
	}
	sess.processChunk(announce("9f3c2a", false), 0)
	if got, ok := publishedHosting(sub); ok {
		t.Errorf("the same client announcing again published %+v, want nothing: the state did not change", got)
	}

	sess.processChunk(announce("9f3c2a", true), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{}) {
		t.Errorf("one end for one client told (%+v, %v), want ({}, true)", got, ok)
	}
}

// An ssh chain announces once per hop, and each hop is its own client.
//
// The count is what the clients act on: each hop arriving or leaving is published even though the aggregate
// stays nested throughout, because a client cannot otherwise tell a press that left a level from one that
// went nowhere. What must not change while any hop remains is the handover itself.
func TestTwoAnnouncedClientsAreDistinct(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sess.processChunk(announce("hop1", false), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{Nested: true, OnlyAnnounced: true, Count: 1}) {
		t.Fatalf("the first hop told (%+v, %v), want ({Nested:true OnlyAnnounced:true Count:1}, true)", got, ok)
	}

	sess.processChunk(announce("hop2", false), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{Nested: true, OnlyAnnounced: true, Count: 2}) {
		t.Fatalf("the second hop told (%+v, %v), want a count of 2", got, ok)
	}

	sess.processChunk(announce("hop2", true), 0)
	got, ok := publishedHosting(sub)
	if !ok || got != (hostingState{Nested: true, OnlyAnnounced: true, Count: 1}) {
		t.Fatalf("the second hop leaving told (%+v, %v), want a count of 1", got, ok)
	}
	if !got.Nested {
		t.Error("the handover lifted while the first hop was still there")
	}

	sess.processChunk(announce("hop1", true), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{}) {
		t.Errorf("after the last hop left, told (%+v, %v), want ({}, true)", got, ok)
	}
}

// A known attachment alongside an announced one takes the prefix key as well.
//
// OnlyAnnounced is what decides whether the outer client keeps its overlay, so it has to follow both
// sources rather than being set once when the first client arrives.
func TestKnownNestingAlongsideAnAnnouncedOneIsPublished(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sess.processChunk(announce("9f3c2a", false), 0)
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{Nested: true, OnlyAnnounced: true, Count: 1}) {
		t.Fatalf("after the announcement, told (%+v, %v), want a count of 1 and announced only", got, ok)
	}

	sess.beginHosting("child")
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{Nested: true, Count: 2}) {
		t.Errorf("with a known child as well, told (%+v, %v), want ({Nested:true Count:2}, true)", got, ok)
	}

	sess.endHosting("child")
	if got, ok := publishedHosting(sub); !ok || got != (hostingState{Nested: true, OnlyAnnounced: true, Count: 1}) {
		t.Errorf("with only the announced one left, told (%+v, %v), want a count of 1 and announced only",
			got, ok)
	}
}

// A client attaching while an announcement is live is told on arrival, including that it was announced.
//
// Same case as the RPC seed: the inner client holds the pty rather than the connection, so it outlives a
// dropped stream. Without carrying OnlyAnnounced the returning window would also give up its overlay,
// which is the one way out of a nesting that is never withdrawn.
func TestAnnouncedNestingIsSeededOnSubscribe(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sess.processChunk(announce("9f3c2a", false), 0)

	sub := sess.subscribeHosting()
	want := hostingState{Nested: true, OnlyAnnounced: true, Count: 1}
	if got, ok := publishedHosting(sub); !ok || got != want {
		t.Errorf("a client attaching while announced-nested was told (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

// A pty read ends where the kernel buffer does, so an announcement arrives split.
//
// processChunk trims a chunk that ends mid-sequence and holds the tail, which is a second mechanism on
// top of the tracker's own. Both have to agree or the announcement is lost exactly when output is busy.
func TestAnnouncementSplitAcrossChunks(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})
	sub := sess.subscribeHosting()

	sequence := announce("9f3c2a", false)
	cut := len(sequence) - 4
	sess.processChunk(sequence[:cut], 0)
	if got, ok := publishedHosting(sub); ok {
		t.Fatalf("half an announcement published %+v, want nothing", got)
	}
	sess.processChunk(sequence[cut:], seq.Shim(cut))

	want := hostingState{Nested: true, OnlyAnnounced: true, Count: 1}
	if got, ok := publishedHosting(sub); !ok || got != want {
		t.Errorf("after the rest arrived, told (%+v, %v), want (%+v, true)", got, ok, want)
	}
}

// Announcements arrive as ordinary output, so anything that prints them is a source: `cat` of a file
// holding one is the honest case. Bounded so that cannot grow without limit.
func TestAnnouncedClientsAreBounded(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	for i := 0; i < maxAnnouncedClients*4; i++ {
		sess.processChunk(announce(fmt.Sprintf("id%d", i), false), 0)
	}

	if got := sess.AnnouncedClients(); got != maxAnnouncedClients {
		t.Errorf("announced clients tracked = %d, want %d", got, maxAnnouncedClients)
	}
}

// The count is what a listing reports, and it has to follow both directions.
//
// Reported at all because an announced client that died with its link never withdrew: the window goes on
// forwarding its detach key, and without this nothing outside says why.
func TestAnnouncedClientsAreCounted(t *testing.T) {
	sess := newNestedTestSession(t, &fakeTerminal{})

	if got := sess.AnnouncedClients(); got != 0 {
		t.Errorf("AnnouncedClients() = %d on a fresh session, want 0", got)
	}
	sess.processChunk(announce("a1", false), 0)
	sess.processChunk(announce("b2", false), 0)
	if got := sess.AnnouncedClients(); got != 2 {
		t.Errorf("AnnouncedClients() = %d with two announced, want 2", got)
	}
	sess.processChunk(announce("a1", true), 0)
	if got := sess.AnnouncedClients(); got != 1 {
		t.Errorf("AnnouncedClients() = %d after one withdrew, want 1", got)
	}
}

// Attribution stays with the parent for an announced client, unlike an RPC-known one.
//
// A deliberate difference, and pinned here so changing it has to argue with a test. The reasoning that
// justifies freezing metadata for a known child does apply, but the derived values are all a local
// consumer has for a session on another host: the OSC 7 that arrives carries the remote host, and
// cm_launch.py reads exactly that to open a split there. See docs/ideas.md on a session's location.
func TestAnnouncedNestingDoesNotFreezeMetadata(t *testing.T) {
	term := &fakeTerminal{}
	sess := newNestedTestSession(t, term)

	sess.processChunk(announce("9f3c2a", false), 0)

	term.mu.Lock()
	term.title = "remote-title"
	term.pwd = "file://white/home/chance"
	term.mu.Unlock()
	sess.noteMetadata()

	title, cwd := sess.Metadata()
	wantCwd := osc.Cwd{Path: "/home/chance", Host: "white", IsLocal: false}
	if title != "remote-title" || !reflect.DeepEqual(cwd, wantCwd) {
		t.Errorf("Metadata() = (%q, %+v), want (%q, %+v): an announced nesting must not freeze what the "+
			"stream reports, or a split loses the host it should open on",
			title, cwd, "remote-title", wantCwd)
	}
}
