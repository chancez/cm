package server

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A program status report (OSC 7501) in a session's output becomes its reported state, kind and all.
//
// Through a real shim and the pump rather than the tracker directly, so the parse, the records and the
// publication are all on the path being asserted.
func TestProgramStatusBecomesTheReportedState(t *testing.T) {
	_, sess := waitFixture(t, "progstatus",
		"printf '\\033]7501;state=blocked:kind=permission:app=terraform:msg=QXBwbHk/\\007MARK\\r\\n'; sleep 5")
	sub := sess.recent.Subscribe(0)
	readUntil(t, sub, "MARK")
	sub.Close()

	want := Reported{State: "blocked", Detail: "Apply?", Source: "terraform", Kind: "permission", Progress: -1}
	checkReported(t, sess.Reported(), want, "")
	if !satisfied(sess, serverv1.WaitState_WAIT_STATE_BLOCKED) {
		t.Error("a session whose program reported blocked does not satisfy a wait for blocked")
	}
}

// A shell prompt ends working and blocked records and keeps done ones, and leaves cm's own report alone.
//
// Each case writes its report and the prompt in one printf, so they normally arrive in one read. That is the
// ordering the pump has to get right: a program that reported working and died, with its shell prompting
// in the same read, must not leave the session working forever.
func TestAPromptEndsALiveProgramStatus(t *testing.T) {
	for _, tc := range []struct {
		name, seq string
		want      Reported
	}{
		{"working", "\\033]7501;state=working\\007", Reported{}},
		{"blocked", "\\033]7501;state=blocked:kind=question\\007", Reported{}},
		{"done survives", "\\033]7501;state=done:app=make\\007", Reported{State: "done", Source: "make", Progress: -1}},
		{"error survives", "\\033]7501;state=error\\007", Reported{State: "error", Progress: -1}},
		{"cm's own report survives", "\\033]25453;state=busy;source=hook\\007",
			Reported{State: "working", Source: "hook", Progress: -1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, sess := waitFixture(t, "prompt", "printf '"+tc.seq+"\\033]133;A\\007MARK\\r\\n'; sleep 5")
			sub := sess.recent.Subscribe(0)
			readUntil(t, sub, "MARK")
			sub.Close()
			checkReported(t, sess.Reported(), tc.want, "")
		})
	}
}

// The program exiting ends its working records, as a prompt does.
func TestAnExitEndsALiveProgramStatus(t *testing.T) {
	_, sess := waitFixture(t, "exitstatus", "printf '\\033]7501;state=working\\007'; sleep 0.2")
	select {
	case <-sess.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("session did not end")
	}
	checkReported(t, sess.Reported(), Reported{}, "a program that has exited is not working")
}

// An adopted session rebuilds its records from the replayed log, prompts included.
//
// The prompt between the two halves is the part a naive recovery gets wrong: taking the last report from
// the log, as cm's own report used to be recovered, would resurrect a record the prompt had already ended.
func TestAdoptionRecoversProgramStatusRecords(t *testing.T) {
	sess := restartAdopting(t, "recordstate",
		"printf '\\033]7501;state=working:id=old\\007'; printf '\\033]133;A\\007'; "+
			"printf '\\033]7501;state=working:app=deploy\\007'; "+
			"printf '\\033]7501;state=blocked:id=eu\\007'; printf 'MARK\\r\\n'; sleep 5",
		"MARK")

	sess.mu.Lock()
	var got []osc.Status
	for _, rec := range sess.records.List() {
		got = append(got, rec.Status)
	}
	sess.mu.Unlock()
	want := []osc.Status{
		{State: "working", App: "deploy", Progress: -1},
		{State: "blocked", ID: "eu", App: "deploy", Progress: -1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("records = %+v, want %+v", got, want)
	}
	checkReported(t, sess.Reported(), Reported{State: "blocked", Source: "deploy", Progress: -1}, "")
}

// cm report carries the protocol's states, kind and progress, and the waits follow them: done satisfies a
// wait for done and one for idle, since a program that has finished is not busy.
func TestReportCarriesProgramStatus(t *testing.T) {
	svc, sess := waitFixture(t, "reportstatus", "sleep 5")
	ctx := context.Background()

	if _, err := svc.Report(ctx, &serverv1.ReportRequest{
		Session:  "reportstatus",
		State:    serverv1.ReportedState_REPORTED_STATE_BLOCKED,
		Detail:   "Apply?",
		Kind:     "permission",
		Progress: new(int32(40)),
	}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	checkReported(t, sess.Reported(), Reported{State: "blocked", Detail: "Apply?", Kind: "permission", Progress: 40}, "")

	// A kind on a state that cannot carry one is dropped, as it would be from the sequence.
	if _, err := svc.Report(ctx, &serverv1.ReportRequest{
		Session:  "reportstatus",
		State:    serverv1.ReportedState_REPORTED_STATE_DONE,
		Kind:     "permission",
		Progress: new(int32(40)),
	}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	checkReported(t, sess.Reported(), Reported{State: "done", Progress: -1}, "")
	for _, tc := range []struct {
		until serverv1.WaitState
		want  bool
	}{
		{serverv1.WaitState_WAIT_STATE_DONE, true},
		{serverv1.WaitState_WAIT_STATE_IDLE, true},
		{serverv1.WaitState_WAIT_STATE_ERROR, false},
		{serverv1.WaitState_WAIT_STATE_BUSY, false},
	} {
		if got := satisfied(sess, tc.until); got != tc.want {
			t.Errorf("satisfied(%v) with a done report = %v, want %v", tc.until, got, tc.want)
		}
	}
}

// An attach gives the client's terminal the session's records, after a clear, ahead of the screen.
//
// The symptom without it: kitty, which keeps records per window, showed none of a session's after an
// attach, because the restore is the screen re-serialized and carries no OSC 7501, and an agent reports
// again only when its state changes. A `cm switch` reattaches in the same terminal without resetting it,
// so the window kept the previous session's records, which is what the clear is for.
//
// Both kinds of record, since `cm report` writes no bytes and this is the only way its state reaches a
// terminal. Neither carries what the protocol has no key for: the report's source.
func TestAttachRestoresProgramStatusRecords(t *testing.T) {
	const partial = "\x1b[38:2:1"
	sess := &Session{
		id:          "attachrecords",
		log:         cmlog.Discard(),
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		term:        &fakeTerminal{restore: []byte("SCREEN"), continuation: []byte(partial)},
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
	}
	sess.setReported(Reported{State: "blocked", Detail: "needs approval", Source: "hook", Progress: -1})
	sess.statuses.Feed([]byte("\x1b]7501;state=working:id=build/test:progress=40:app=make\x07"))
	sess.noteStatus()
	sess.recent.Append([]byte(partial))
	sess.feedTerminal([]byte(partial), sess.recent.Next())

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	want := "\x1b]7501;state=clear\a" +
		"\x1b]7501;state=blocked:msg=bmVlZHMgYXBwcm92YWw=\a" +
		"\x1b]7501;state=working:id=build/test:progress=40:app=make\a" +
		"SCREEN" + partial
	if got := string(att.restore); got != want {
		t.Errorf("restore = %q, want %q", got, want)
	}
}
