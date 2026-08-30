package server

import (
	"context"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A reported state must be visible and take precedence over what cm derives from the shell.
//
// The precedence is the point rather than a detail. A coding agent runs as one long-lived command, so the
// shell reports "busy" from the moment it starts until it exits, while the agent itself moves between
// working, waiting for an answer, and finished. Deriving state from the shell cannot see any of that.
func TestReportTakesPrecedenceOverTheDerivedState(t *testing.T) {
	svc, sess := waitFixture(t, "reported", "sleep 5")
	ctx := context.Background()

	// The shell says a command is running, which is what an agent looks like from outside.
	setBusy(sess, true, "claude")
	if !sess.Command().Running {
		t.Fatal("setup: want the shell reporting a running command")
	}

	// The program says it is actually waiting for input.
	if _, err := svc.Report(ctx, &serverv1.ReportRequest{
		Session: "reported",
		State:   serverv1.ReportedState_REPORTED_STATE_BLOCKED,
		Detail:  "needs approval",
		Source:  "my-agent",
	}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}

	want := Reported{State: "blocked", Detail: "needs approval", Source: "my-agent"}
	checkReported(t, sess.Reported(), want, "")

	// A wait for blocked is satisfied by the report, and a wait for busy is not: the report replaces the
	// derived state rather than adding to it.
	if !satisfied(sess, serverv1.WaitState_WAIT_STATE_BLOCKED) {
		t.Error("a session reported blocked does not satisfy a wait for blocked")
	}
	if satisfied(sess, serverv1.WaitState_WAIT_STATE_BUSY) {
		t.Error("a session reported blocked satisfies a wait for busy, want the report to take over " +
			"from the shell's view that a command is running")
	}
}

// Clearing a report falls back to what cm derives, rather than leaving the session stateless.
func TestReportClearFallsBackToTheDerivedState(t *testing.T) {
	svc, sess := waitFixture(t, "cleared", "sleep 5")
	ctx := context.Background()

	setBusy(sess, true, "make")
	if _, err := svc.Report(ctx, &serverv1.ReportRequest{
		Session: "cleared",
		State:   serverv1.ReportedState_REPORTED_STATE_IDLE,
	}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}
	// The report says idle even though the shell says busy.
	if satisfied(sess, serverv1.WaitState_WAIT_STATE_BUSY) {
		t.Fatal("setup: the report should have taken precedence")
	}

	if _, err := svc.Report(ctx, &serverv1.ReportRequest{
		Session: "cleared",
		State:   serverv1.ReportedState_REPORTED_STATE_CLEAR,
	}); err != nil {
		t.Fatalf("Report(clear) error = %v", err)
	}

	if got := sess.Reported(); got != (Reported{}) {
		t.Errorf("Reported() = %+v after clearing, want empty", got)
	}
	// And the derived state is back in charge.
	if !satisfied(sess, serverv1.WaitState_WAIT_STATE_BUSY) {
		t.Error("clearing a report did not fall back to the shell's state, which still says busy")
	}
}

// A wait for blocked must be released by a report arriving, not only satisfied by one already present.
//
// This is what a script coordinating with an agent does: start it working, then block until it needs
// something. Nothing else in cm can produce this state, so without the report the wait can only time out.
func TestWaitForBlockedIsReleasedByAReport(t *testing.T) {
	svc, sess := waitFixture(t, "release", "sleep 5")
	ctx := context.Background()

	setBusy(sess, true, "claude")

	done := make(chan *serverv1.WaitResponse, 1)
	go func() {
		resp, err := svc.Wait(ctx, &serverv1.WaitRequest{
			Session:   "release",
			Until:     serverv1.WaitState_WAIT_STATE_BLOCKED,
			TimeoutMs: 10_000,
		})
		if err != nil {
			t.Errorf("Wait() error = %v", err)
			done <- nil
			return
		}
		done <- resp
	}()

	// Nothing has reported, so the wait must not resolve: a shell cannot express blocked.
	select {
	case resp := <-done:
		t.Fatalf("Wait(blocked) returned %+v before anything reported being blocked", resp)
	case <-time.After(200 * time.Millisecond):
	}

	if _, err := svc.Report(ctx, &serverv1.ReportRequest{
		Session: "release",
		State:   serverv1.ReportedState_REPORTED_STATE_BLOCKED,
		Detail:  "which branch?",
	}); err != nil {
		t.Fatalf("Report() error = %v", err)
	}

	select {
	case resp := <-done:
		if resp == nil || !resp.Satisfied {
			t.Fatalf("Wait(blocked) = %+v, want satisfied once a report arrived", resp)
		}
		// The detail comes back, since a caller that waited for blocked wants to know what for.
		if resp.ReportedState != "blocked" || resp.ReportedDetail != "which branch?" {
			t.Errorf("Wait() = %+v, want the reported state and detail", resp)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait(blocked) was not released by a report")
	}
}

// Reporting is not agent-specific, and the server does not care what is reporting.
//
// Asserted because it is the design decision that keeps this from rotting: herdr's fallback is a TOML
// manifest of regexes per agent, versioned and updated as each agent's UI changes. cm knows nothing about
// what runs in a session, so an unrecognized reporter works exactly as well as a known one.
func TestReportAcceptsAnySource(t *testing.T) {
	svc, sess := waitFixture(t, "anysource", "sleep 5")
	ctx := context.Background()

	for _, source := range []string{"", "make", "some-tool-cm-has-never-heard-of", "🤖"} {
		if _, err := svc.Report(ctx, &serverv1.ReportRequest{
			Session: "anysource",
			State:   serverv1.ReportedState_REPORTED_STATE_BUSY,
			Detail:  "working",
			Source:  source,
		}); err != nil {
			t.Errorf("Report(source=%q) error = %v", source, err)
			continue
		}
		if got := sess.Reported().Source; got != source {
			t.Errorf("Source = %q, want %q", got, source)
		}
	}
}

// A report about a session that is not running fails rather than being silently dropped.
func TestReportOnUnknownSessionFails(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	svc := NewService(mgr)

	if _, err := svc.Report(context.Background(), &serverv1.ReportRequest{
		Session: "nope",
		State:   serverv1.ReportedState_REPORTED_STATE_BUSY,
	}); err == nil {
		t.Error("Report() on an unknown session succeeded, want an error")
	}
}

// A report with no state is refused, since every value means something different and guessing would
// record something the caller did not say.
func TestReportRequiresAState(t *testing.T) {
	svc, _ := waitFixture(t, "nostate", "sleep 5")

	if _, err := svc.Report(context.Background(), &serverv1.ReportRequest{
		Session: "nostate",
	}); err == nil {
		t.Error("Report() with no state succeeded, want an error")
	}
}

// checkReported asserts a session's report as a whole value against a want carrying no timestamp.
//
// The timestamp comes from the clock inside setReported, so no fixture can predict it. Checked for being
// set, then folded into want so the comparison stays whole: comparing three fields by hand instead would
// pass while the fourth was wrong, which is the failure the whole-value rule exists for.
func checkReported(t *testing.T, got, want Reported, note string) {
	t.Helper()
	if got.State != "" && got.At.IsZero() {
		t.Errorf("Reported() = %+v with no timestamp, so its age could never be shown", got)
	}
	want.At = got.At
	if got == want {
		return
	}
	if note != "" {
		t.Errorf("Reported() = %+v, want %+v: %s", got, want, note)
		return
	}
	t.Errorf("Reported() = %+v, want %+v", got, want)
}
