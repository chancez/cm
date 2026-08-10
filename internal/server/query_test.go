package server

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/shim"
)

// A terminal query must not reach an attached client, because cm's own emulator answers it.
//
// This is the regression test for "exiting vim leaves garbage below my prompt". A program asked the
// terminal a question, cm answered it, and the query also travelled out to kitty, which answered
// too. The program read the first reply and exited, so the second arrived with nobody waiting and
// zsh's line editor printed it at the prompt as "62;52;c": a DA1 reply minus the ESC [ that a split
// read had eaten.
//
// Asserted at the seam rather than through a real terminal. The bug is entirely about which bytes
// reach a client, which is observable here, and an end-to-end version needs a pty willing to answer
// queries. internal/e2e/query_test.go covers that half.
func TestQueriesAnsweredByCMDoNotReachClients(t *testing.T) {
	// Printed by the shell around the query so the assertion can prove the surrounding output still
	// arrives. A test that only checked for the query's absence would pass if nothing arrived at all.
	rec := startShimFor(t, shim.Config{
		Session: "query",
		Command: []string{"/bin/sh", "-c", `printf 'BEFORE\033[cAFTER\n'; sleep 5`},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// Waiting for the text after the query means the query itself has certainly been processed, so
	// its absence below is a real absence rather than a race with delivery.
	got := readUntil(t, att.reader, "AFTER")

	if strings.Contains(got, "\x1b[c") {
		t.Errorf("client received %q, which still contains the DA1 query \\x1b[c.\n"+
			"cm's emulator answers this query, so a copy reaching the real terminal means the program "+
			"that asked gets two replies and the spare one is printed at the shell's prompt.", got)
	}

	// The output around the query must survive, or the strip is eating real bytes.
	if !strings.Contains(got, "BEFORE") || !strings.Contains(got, "AFTER") {
		t.Errorf("client received %q, want it to contain both %q and %q: stripping the query must not "+
			"remove the output around it", got, "BEFORE", "AFTER")
	}

	// The terminal model is what answers the query, so it must still see the original bytes. Feeding
	// it the stripped copy would mean nothing answers and the program hangs, which is worse than the
	// artifact this fixes.
	if written := term.Written(); !strings.Contains(written, "\x1b[c") {
		t.Errorf("terminal model was fed %q, want it to contain the DA1 query \\x1b[c.\n"+
			"The model generates the reply that drainPending delivers, so stripping the query before "+
			"the model sees it would leave the query unanswered entirely.", written)
	}
}

// A query split across two shim reads must still be stripped.
//
// Without holding the fragment, both halves reach the client, the real terminal reassembles the query
// and answers it, and the bug returns as an intermittent one. Driven through the pump directly so the
// split lands at a chosen byte instead of wherever a pty read happens to break.
func TestQuerySplitAcrossReadsIsStripped(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "split",
		Command: []string{"/bin/sh", "-c", "sleep 5"},
		Rows:    24, Cols: 80,
	})

	term := &fakeTerminal{restore: []byte("RESTORED")}
	sess, err := newSession(rec, term, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	att, err := sess.attach(nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// stripQueries is what the pump calls, so this exercises the real path including the carried
	// fragment. Safe to call directly here because this session's pump is parked on a sleeping shell
	// and is producing nothing.
	first := sess.stripQueries([]byte("BEFORE\x1b"))
	second := sess.stripQueries([]byte("[cAFTER"))

	joined := string(first) + string(second)
	if strings.Contains(joined, "\x1b[c") {
		t.Errorf("a query split across two reads came through as %q, which still contains \\x1b[c.\n"+
			"The fragment must be held until the rest arrives, or the real terminal reassembles the "+
			"query and answers it.", joined)
	}
	if joined != "BEFOREAFTER" {
		t.Errorf("stripped output = %q, want %q: the bytes around a split query must survive intact",
			joined, "BEFOREAFTER")
	}
}
