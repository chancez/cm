// Package progstatus keeps a session's program status records, the state the program status protocol
// (OSC 7501) describes.
//
// The protocol leaves storage to the terminal and states the rules a terminal must follow: one record per
// id, a report replaces its record whole, a clear removes a record and everything beneath it, a record
// without an app inherits its nearest ancestor's, at most 256 are kept, and working and blocked records
// end at the next shell prompt or when the program exits. Those rules live here, apart from the server,
// so they can be tested without one.
package progstatus

import (
	"slices"
	"strings"
	"time"

	"github.com/chancez/cm/internal/osc"
)

// MaxRecords is how many records a session keeps. The specification's ceiling, and four times its floor.
const MaxRecords = 256

// Record is what the most recent report about one id said.
type Record struct {
	osc.Status
	// Legacy marks a record that came from cm's own OSC 25453 report rather than from OSC 7501.
	//
	// Such a record is exempt from the prompt and exit rules, because cm's own report never had them: a
	// shell integration loaded before this build keeps writing 25453 until that shell exits, and its
	// reports have to keep meaning what they meant when it was loaded.
	Legacy bool
	// Source names the reporter, free-form, for a record from `cm report` or OSC 25453. Those never
	// restricted it, and cm does not care what reports, so it is carried beside App rather than squeezed
	// into one. Empty for OSC 7501.
	Source string
	// At is when the report was made.
	At time.Time

	// order is when the record was last written, relative to the others, which decides both which record
	// is evicted at the limit and which of two equally urgent records summarizes the session.
	order uint64
}

// Records is a session's program status records.
//
// Not safe for concurrent use. The server holds it under the session's lock.
type Records struct {
	recs  map[string]Record
	clock uint64
}

// Apply records a report, reporting whether anything changed. A "clear" removes records rather than
// writing one.
func (r *Records) Apply(rec Record) bool {
	if rec.State == "clear" {
		return r.clear(rec.ID)
	}
	if r.recs == nil {
		r.recs = make(map[string]Record)
	}
	// A repeat leaves the record alone, its time included. A program that restates its state on every
	// prompt has not just made the statement, and the age is what separates a fresh report from one
	// nobody has retracted.
	prev, had := r.recs[rec.ID]
	if had && prev.Status == rec.Status && prev.Legacy == rec.Legacy && prev.Source == rec.Source {
		return false
	}
	r.clock++
	rec.order = r.clock
	r.recs[rec.ID] = rec
	if !had && len(r.recs) > MaxRecords {
		r.evictOldest()
	}
	return true
}

// clear removes the record with this id and every record beneath it. An empty id removes every record.
func (r *Records) clear(id string) bool {
	n := len(r.recs)
	for k := range r.recs {
		if id == "" || k == id || strings.HasPrefix(k, id+"/") {
			delete(r.recs, k)
		}
	}
	return len(r.recs) != n
}

// evictOldest removes the record written longest ago.
func (r *Records) evictOldest() {
	var oldest string
	var at uint64
	first := true
	for k, rec := range r.recs {
		if first || rec.order < at {
			oldest, at, first = k, rec.order, false
		}
	}
	delete(r.recs, oldest)
}

// Restamp sets when a record's report was made. For a record rebuilt from history, whose real time is
// known from somewhere the history is not.
func (r *Records) Restamp(id string, at time.Time) {
	if rec, ok := r.recs[id]; ok {
		rec.At = at
		r.recs[id] = rec
	}
}

// Ended applies the rule for a shell prompt starting or the program exiting: working and blocked records
// are removed, since whatever was working or waiting is no longer running. done, error and idle stay,
// done and error because the specification keeps them until someone has seen them. Reports whether
// anything changed.
func (r *Records) Ended() bool {
	n := len(r.recs)
	for k, rec := range r.recs {
		if !rec.Legacy && (rec.State == "working" || rec.State == "blocked") {
			delete(r.recs, k)
		}
	}
	return len(r.recs) != n
}

// List returns every record, ordered by id so the root comes first and children follow their parents,
// with each app inherited from the nearest ancestor that has one.
func (r *Records) List() []Record {
	out := make([]Record, 0, len(r.recs))
	for _, rec := range r.recs {
		rec.App = r.app(rec.ID, rec.App)
		out = append(out, rec)
	}
	slices.SortFunc(out, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	return out
}

// app resolves a record's app, walking up its ancestors when it has none of its own. The root, whose id is
// empty, is every record's last ancestor.
func (r *Records) app(id, app string) string {
	for app == "" && id != "" {
		if i := strings.LastIndexByte(id, '/'); i >= 0 {
			id = id[:i]
		} else {
			id = ""
		}
		app = r.recs[id].App
	}
	return app
}

// urgency orders states by how much a person needs to know about them. A session is summarized by its most
// urgent record, so one blocked child is what the session shows while its siblings work.
var urgency = map[string]int{
	"blocked": 5,
	"error":   4,
	"working": 3,
	"done":    2,
	"idle":    1,
}

// Summary returns the record that best describes the session as a whole: the most urgent, and of equally
// urgent records the most recently written. False when there are none.
func (r *Records) Summary() (Record, bool) {
	var best Record
	found := false
	for _, rec := range r.recs {
		if !found || urgency[rec.State] > urgency[best.State] ||
			(urgency[rec.State] == urgency[best.State] && rec.order > best.order) {
			best, found = rec, true
		}
	}
	if found {
		best.App = r.app(best.ID, best.App)
	}
	return best, found
}
