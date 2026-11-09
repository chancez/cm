package server

import (
	"slices"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
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
