package progstatus

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/chancez/cm/internal/osc"
)

var t0 = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

func st(state, id, app string) osc.Status {
	return osc.Status{State: state, ID: id, App: app, Progress: -1}
}

func rec(state, id, app string, legacy bool) Record {
	return Record{Status: st(state, id, app), Legacy: legacy, At: t0}
}

// list returns r.List() with the write order zeroed, so a whole record can be compared against a literal.
func list(r *Records) []Record {
	out := r.List()
	for i := range out {
		out[i].order = 0
	}
	return out
}

// summary is r.Summary() with the write order zeroed, for the same reason.
func summary(r *Records) Record {
	s, _ := r.Summary()
	s.order = 0
	return s
}

// A report replaces its record whole: a key it leaves out is gone, not kept from before.
func TestApplyReplacesTheWholeRecord(t *testing.T) {
	var r Records
	r.Apply(Record{Status: osc.Status{State: "working", Progress: 40, Message: "building", Title: "Build"}, At: t0})
	r.Apply(Record{Status: st("done", "", ""), At: t0})
	want := []Record{rec("done", "", "", false)}
	if got := list(&r); !slices.Equal(got, want) {
		t.Errorf("List() mismatch: got %+v, want %+v", got, want)
	}
}

// A clear removes its record and everything beneath it, and an empty id removes everything. "build" must
// not take "builder" with it: beneath means a "/" boundary, not a string prefix.
func TestClearRemovesTheRecordAndItsChildren(t *testing.T) {
	var r Records
	for _, id := range []string{"", "build", "build/test", "build/test/unit", "builder"} {
		r.Apply(Record{Status: st("working", id, ""), At: t0})
	}
	if !r.Apply(Record{Status: st("clear", "build", ""), At: t0}) {
		t.Error("clearing an existing record reported no change")
	}
	want := []Record{rec("working", "", "", false), rec("working", "builder", "", false)}
	if got := list(&r); !slices.Equal(got, want) {
		t.Errorf("after clear build: got %+v, want %+v", got, want)
	}
	if r.Apply(Record{Status: st("clear", "nothing", ""), At: t0}) {
		t.Error("clearing a missing record reported a change")
	}
	r.Apply(Record{Status: st("clear", "", ""), At: t0})
	if got := list(&r); !slices.Equal(got, []Record{}) {
		t.Errorf("after clear all: got %+v, want %+v", got, []Record{})
	}
}

// A record without an app takes its nearest ancestor's, and the root counts as everyone's ancestor.
func TestAppIsInheritedFromTheNearestAncestor(t *testing.T) {
	var r Records
	r.Apply(Record{Status: st("working", "", "deploy"), At: t0})
	r.Apply(Record{Status: st("working", "eu", "terraform"), At: t0})
	r.Apply(Record{Status: st("working", "eu/db", ""), At: t0})
	r.Apply(Record{Status: st("working", "us/web", ""), At: t0})
	want := []Record{
		rec("working", "", "deploy", false),
		rec("working", "eu", "terraform", false),
		rec("working", "eu/db", "terraform", false),
		rec("working", "us/web", "deploy", false),
	}
	if got := list(&r); !slices.Equal(got, want) {
		t.Errorf("List() mismatch: got %+v, want %+v", got, want)
	}
}

// At the limit, a new record evicts the one written longest ago, and rewriting a record makes it new.
func TestTheOldestRecordIsEvictedAtTheLimit(t *testing.T) {
	var r Records
	for i := range MaxRecords {
		r.Apply(Record{Status: st("working", fmt.Sprint(i), ""), At: t0})
	}
	r.Apply(Record{Status: st("done", "0", ""), At: t0})
	r.Apply(Record{Status: st("working", "new", ""), At: t0})

	got := map[string]bool{}
	for _, rec := range r.List() {
		got[rec.ID] = true
	}
	if len(got) != MaxRecords || !got["0"] || got["1"] || !got["new"] {
		t.Errorf("after overflow: %d records, has 0=%v 1=%v new=%v; want %d, true, false, true",
			len(got), got["0"], got["1"], got["new"], MaxRecords)
	}
}

// A prompt or an exit ends working and blocked records and keeps the rest, and a record from cm's own
// report is not touched, since that report never had the rule.
func TestEndedDropsOnlyLiveRecords(t *testing.T) {
	var r Records
	r.Apply(Record{Status: st("working", "a", ""), At: t0})
	r.Apply(Record{Status: st("blocked", "b", ""), At: t0})
	r.Apply(Record{Status: st("done", "c", ""), At: t0})
	r.Apply(Record{Status: st("error", "d", ""), At: t0})
	r.Apply(Record{Status: st("idle", "e", ""), At: t0})
	r.Apply(Record{Status: st("blocked", "", ""), At: t0, Legacy: true})
	if !r.Ended() {
		t.Error("Ended() reported no change")
	}
	want := []Record{
		rec("blocked", "", "", true),
		rec("done", "c", "", false),
		rec("error", "d", "", false),
		rec("idle", "e", "", false),
	}
	if got := list(&r); !slices.Equal(got, want) {
		t.Errorf("List() mismatch: got %+v, want %+v", got, want)
	}
	if r.Ended() {
		t.Error("second Ended() reported a change")
	}
}

// The summary is the most urgent record, the most recent of equals, with its app resolved.
func TestSummaryIsTheMostUrgentRecord(t *testing.T) {
	var r Records
	if got, ok := r.Summary(); ok {
		t.Errorf("Summary() of nothing = %+v, true", got)
	}
	r.Apply(Record{Status: st("working", "", "deploy"), At: t0})
	r.Apply(Record{Status: st("blocked", "eu", ""), At: t0})
	r.Apply(Record{Status: st("working", "us", ""), At: t0})
	if got, want := summary(&r), rec("blocked", "eu", "deploy", false); got != want {
		t.Errorf("Summary() = %+v, want %+v", got, want)
	}

	r.Apply(Record{Status: st("blocked", "us", ""), At: t0})
	if got, want := summary(&r), rec("blocked", "us", "deploy", false); got != want {
		t.Errorf("Summary() of two blocked = %+v, want %+v", got, want)
	}
}

// Writing the same thing twice is not a change, which is what keeps a repeating reporter from satisfying
// every wait issued against it.
func TestApplyReportsChange(t *testing.T) {
	var r Records
	if !r.Apply(Record{Status: st("working", "", ""), At: t0}) {
		t.Error("first report reported no change")
	}
	if r.Apply(Record{Status: st("working", "", ""), At: t0.Add(time.Second)}) {
		t.Error("repeated report reported a change")
	}
	if got := list(&r); !slices.Equal(got, []Record{rec("working", "", "", false)}) {
		t.Errorf("after a repeat: got %+v, want the first report's time kept", got)
	}
	if !r.Apply(Record{Status: st("working", "", ""), At: t0, Legacy: true}) {
		t.Error("same report from the other protocol reported no change")
	}
	if !r.Apply(Record{Status: st("working", "", ""), At: t0, Legacy: true, Source: "hook"}) {
		t.Error("same report from another reporter reported no change")
	}
}
