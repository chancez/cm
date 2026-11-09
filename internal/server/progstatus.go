package server

import (
	"time"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/progstatus"
)

// applyStatusChunk applies what one chunk of output said about the program's status, reporting whether any
// record changed. Shared by the live pump and the adoption replay, so a restart rebuilds exactly the records
// the previous server held.
//
// cm's own report goes first, then the program status events in the order the stream carried them. Only the
// second order matters: a prompt ends what was reported before it and nothing after, and the legacy record is
// exempt from prompts, so where it falls among them changes nothing.
func applyStatusChunk(recs *progstatus.Records, legacy *osc.Report, events []osc.StatusEvent, at time.Time) bool {
	changed := false
	if legacy != nil {
		changed = applyLegacyReport(recs, *legacy, at) || changed
	}
	for _, e := range events {
		if e.Prompt {
			changed = recs.Ended() || changed
			continue
		}
		changed = recs.Apply(progstatus.Record{Status: e.Status, At: at}) || changed
	}
	return changed
}

// applyLegacyReport applies cm's own OSC 25453 report as a report about the root record.
//
// Its "clear" withdrew the session's only report, so it clears every record: with one record per session
// the two meant the same thing.
func applyLegacyReport(recs *progstatus.Records, r osc.Report, at time.Time) bool {
	return recs.Apply(recordFromReport(r.State, r.Detail, r.Source, at, true))
}
