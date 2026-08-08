package server

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chancez/cm/internal/paths"
)

// FindingLogWarnings is warnings in cm's logs, which are quieter than errors and often more useful.
const FindingLogWarnings = "log-warnings"

// maxLoggedLines bounds how many log lines a finding quotes.
//
// A bound rather than all of them: a server up for weeks can have hundreds, and a diagnostic that prints
// hundreds of lines is one nobody reads. The full log is available through `cm logs`.
const maxLoggedLines = 5

// logStaleAfter is how old a log entry has to be before it stops being reported.
//
// The reason for a window at all was found by running the command: it reported three errors about sessions
// named kitty.1 and kitty.5 that had been dead for nineteen hours, across six server generations sharing one
// appended file. That is not a diagnosis, it is history, and a check that reports last week's resolved
// problems on every run is one people learn to ignore.
//
// A day rather than an hour. cm's whole point is that sessions and their problems outlive a terminal window,
// so an error from this morning is still worth seeing this evening; one from last week is not.
const logStaleAfter = 24 * time.Hour

// logStaleAfterText is how the window is described in a finding.
//
// Spelled out rather than taken from Duration.String, which renders this as "24h0m0s". A diagnostic is read by
// a person, and "24h0m0s" is the sort of detail that makes output look machine-generated and unconsidered.
const logStaleAfterText = "24 hours"

// logEntry is one parsed log line worth reporting.
type logEntry struct {
	when  time.Time
	level string
	file  string
	text  string
}

// checkLogs reports recent errors and warnings across cm's logs.
//
// The log is where cm records what it could not act on: a terminal model that failed and disabled screen
// restore for a session, a store write that did not land, a session that should have been reaped and was not.
// Those are deliberately not shown in the terminal, because the alternative is interrupting whatever the user
// is doing, and the consequence is that nobody ever looks.
//
// This replaced a narrower version, and each difference came from watching it be wrong:
//
//   - Warnings are reported too, separately from errors. cm logs 22 warnings and 9 errors, and the warnings
//     are the substantive ones: "adopting session failed", "rebuilding the screen for an adopted session
//     failed", "replaying persisted session failed". Those are exactly the silent failures behind the
//     blank-screen-on-reattach bug, and the old check could not see any of them.
//   - Shim logs are read, not just the server's. A shim logs "persisting output failed, session will not
//     survive a reboot", which is precisely the kind of thing this command exists for, into a file nothing
//     opened.
//   - Old entries are skipped. See logStaleAfter.
//
// Errors and warnings are separate findings rather than one, so a script can gate on errors while merely
// noticing warnings, and so a pile of warnings cannot hide a single error in a truncated list.
func (m *Manager) checkLogs(now time.Time) []Finding {
	var entries []logEntry
	for _, path := range m.logFiles() {
		entries = append(entries, scanLog(path, now)...)
	}
	if len(entries) == 0 {
		return nil
	}

	// Newest first. When the list is truncated, the most recent entries are the ones that explain what is
	// happening now; the old check showed the first few, which after a rotation are the least relevant.
	sort.Slice(entries, func(i, j int) bool { return entries[i].when.After(entries[j].when) })

	var findings []Finding
	for _, group := range []struct {
		level string
		kind  string
		what  string
		note  string
	}{
		{
			level: "ERROR", kind: FindingServerErrors, what: "error",
			note: "each is something cm could not do and did not interrupt you about",
		},
		{
			level: "WARN", kind: FindingLogWarnings, what: "warning",
			note: "a warning is a degraded session rather than a failed one: screen restore disabled, " +
				"output not persisted, a size not recorded",
		},
	} {
		var (
			quoted []string
			total  int
		)
		for _, e := range entries {
			if e.level != group.level {
				continue
			}
			total++
			if len(quoted) < maxLoggedLines {
				quoted = append(quoted, fmt.Sprintf("%s: %s", e.file, e.text))
			}
		}
		if total == 0 {
			continue
		}

		// Pluralized properly rather than with "(s)", for the same reason the window is spelled out: this is
		// read by a person.
		noun := group.what
		if total != 1 {
			noun += "s"
		}
		detail := fmt.Sprintf("%d %s in the last %s, newest first; %s",
			total, noun, logStaleAfterText, group.note)
		if total > len(quoted) {
			detail += fmt.Sprintf(" (showing %d)", len(quoted))
		}
		findings = append(findings, Finding{
			Kind:   group.kind,
			Detail: detail + ":\n  " + strings.Join(quoted, "\n  "),
			// Not fixable: a log entry records something that already happened, and deleting the log would
			// destroy the evidence rather than fix anything.
			Fixable: false,
		})
	}
	return findings
}

// logFiles lists the log files worth scanning.
//
// Includes each shim's log, which the previous version of this check ignored entirely. A shim is the process
// holding the pty, so its failures are the ones a user feels, and they were being written to a file nothing
// read.
//
// Rotated generations are skipped on purpose. cmlog keeps one previous file, which exists so a problem that
// just scrolled past is still recoverable by hand; including it here would mostly resurface entries already
// outside the staleness window.
func (m *Manager) logFiles() []string {
	files := []string{m.dirs.ServerLog()}

	// Every shim log present, rather than one per known session: a shim whose session record is gone is
	// exactly the case where its log is the only remaining account of what happened.
	dir := filepath.Dir(m.dirs.ServerLog())
	entries, err := os.ReadDir(dir)
	if err != nil {
		return files
	}
	var shims []string
	for _, e := range entries {
		name := e.Name()
		// Matched against the naming ShimLog produces rather than a literal, so a change there cannot
		// silently stop this from finding anything.
		if strings.HasPrefix(name, shimLogPrefix) && strings.HasSuffix(name, shimLogSuffix) {
			shims = append(shims, filepath.Join(dir, name))
		}
	}
	sort.Strings(shims)
	return append(files, shims...)
}

// scanLog returns the recent errors and warnings in one log file.
func scanLog(path string, now time.Time) []logEntry {
	f, err := os.Open(path)
	if err != nil {
		// No log is not a problem: a server that has just started may not have written one.
		return nil
	}
	defer f.Close()

	name := filepath.Base(path)
	var out []logEntry
	sc := bufio.NewScanner(f)
	// A long line is possible, since a logged error can embed a command line or a path.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())

		// Matched on the level attribute rather than on the word "error" anywhere in the line, so a session
		// named "error-repro" or a command containing it does not produce a finding.
		var level string
		switch {
		case strings.Contains(line, "level=ERROR"):
			level = "ERROR"
		case strings.Contains(line, "level=WARN"):
			level = "WARN"
		default:
			continue
		}

		when, ok := logStamp(line)
		if !ok {
			// Unparseable timestamp. Reported rather than dropped: an entry at the right level is worth
			// seeing, and silently discarding it because of its timestamp would be the worse failure. Left as
			// the zero time, which sorts last.
			out = append(out, logEntry{level: level, file: name, text: line})
			continue
		}
		if now.Sub(when) > logStaleAfter {
			continue
		}
		out = append(out, logEntry{when: when, level: level, file: name, text: line})
	}
	return out
}

// logStamp recovers the timestamp from a log line.
//
// Parsed from the text rather than taken from the file's mtime, which would date every entry in a file by its
// last write. slog's TextHandler puts time first as RFC3339 with nanoseconds; a whole-second stamp parses too,
// and splitting on the first space is safe because a quoted message with spaces comes later in the line.
// Verified against real lines from both a server and a shim log.
func logStamp(line string) (time.Time, bool) {
	const key = "time="
	if !strings.HasPrefix(line, key) {
		return time.Time{}, false
	}
	rest := line[len(key):]
	end := strings.IndexByte(rest, ' ')
	if end < 0 {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, rest[:end])
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Shim log naming, derived from the function that produces it rather than duplicated as literals.
var shimLogPrefix, shimLogSuffix = shimLogAffixes()

// shimLogAffixes splits a sample shim log name into the parts around the session name.
func shimLogAffixes() (prefix, suffix string) {
	const sample = "\x00session\x00"
	d := paths.Dirs{State: "/"}
	base := filepath.Base(d.ShimLog(sample))
	parts := strings.SplitN(base, sample, 2)
	if len(parts) != 2 {
		// Unreachable unless ShimLog stops embedding the name, in which case scanning cannot work and failing
		// loudly beats silently finding nothing.
		panic("cm: shim log naming no longer embeds the session name")
	}
	return parts[0], parts[1]
}
