package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// sessionJSON is the JSON shape of a session.
//
// Declared here rather than marshalling the protobuf message directly, for two reasons. The wire
// message carries fields that exist only for compatibility, such as `exited` alongside `state`, and
// exposing both would invite scripts to depend on the one being phased out. And a hand-written
// struct makes the output a deliberate contract: adding a wire field cannot silently change what
// scripts see.
//
// Fields are only ever added, never renamed or removed, so a script that reads one keeps working.
type sessionJSON struct {
	Name string `json:"name"`
	// State is "running", "exited", or "dead". Prefer it over exit_code alone: a dead session's
	// outcome is unknown rather than zero.
	State string `json:"state"`
	// ShellPID is 0 once the shell has exited.
	ShellPID int32 `json:"shell_pid"`
	Clients  int32 `json:"clients"`
	// ExitCode is meaningful only when state is "exited".
	ExitCode int32  `json:"exit_code"`
	Title    string `json:"title"`
	// Cwd is the decoded local path, empty when the session reported a directory on another host.
	// Acting on a remote path locally would open the wrong place or fail, so it is withheld rather
	// than handed over.
	Cwd string `json:"cwd"`
	// CwdURI is the directory as the shell reported it, keeping the host. Present even when Cwd is
	// empty, so a caller can tell "no directory reported" from "directory is remote".
	CwdURI string `json:"cwd_uri"`
	// CwdIsLocal reports whether Cwd refers to this machine.
	CwdIsLocal bool `json:"cwd_is_local"`
	// CreatedAt is an RFC 3339 timestamp, which sorts lexically and needs no timezone guessing.
	CreatedAt string `json:"created_at"`
	// CreatedAtUnix is the same instant in seconds, for callers that would otherwise parse the
	// string back.
	CreatedAtUnix int64 `json:"created_at_unix"`
	// Busy reports whether a command is running rather than the shell sitting at a prompt, from
	// OSC 133.
	//
	// What a terminal emulator needs to decide whether closing a window is destructive: the shell owns
	// the pty, so the emulator only ever sees `cm attach` running and cannot tell busy from idle.
	// False for a shell that does not report OSC 133, since nothing then says otherwise.
	Busy bool `json:"busy"`
	// Command is the command line the shell reported running, when it reported one. Can be empty
	// while Busy is true, since the cmdline parameter is an extension not every shell sends.
	Command string `json:"command"`
	// LastCommandExitCode is the status of the last command the shell finished, from OSC 133;D.
	//
	// Distinct from ExitCode, which is the session's own: that says whether the shell has gone, this says
	// whether the last thing it ran succeeded. Conflating them would report a failed build as a dead
	// session.
	//
	// CommandFinished is what makes the value readable, since zero is a real status and cannot double as
	// "nothing has finished". False for a shell whose integration sends a bare 133;D with no status, which
	// is legal.
	LastCommandExitCode int32 `json:"last_command_exit_code"`
	CommandFinished     bool  `json:"command_finished"`
	// ReportedState is what a program in the session said about itself via `cm report`: "idle", "busy",
	// or "blocked". Empty when nothing has reported.
	//
	// Takes precedence over Busy, which is derived from the shell. A program describing itself is better
	// evidence, and "blocked" cannot be derived at all: a shell marks a command as running whether it is
	// computing or waiting at a prompt of its own.
	ReportedState string `json:"reported_state"`
	// ReportedDetail and ReportedSource are the note that came with the report and who sent it. Both
	// free-form and optional.
	ReportedDetail string `json:"reported_detail"`
	ReportedSource string `json:"reported_source"`
	// Tags are the caller's own labels for this session, set at creation or with `cm tag`.
	//
	// A map rather than the "k=v,k" string the table prints, since a script filtering on one key
	// should not have to parse a rendering meant for a column. Always present, and empty rather than
	// null for a session with no tags, so `jq '.[].tags.project'` does not have to special-case it.
	Tags map[string]string `json:"tags"`
}

// toSessionJSON converts a wire session for output.
func toSessionJSON(s *serverv1.Session) sessionJSON {
	created := time.Unix(s.CreatedAtUnix, 0)

	// A remote directory is reported as no local path rather than as one that does not exist here,
	// so a caller cannot act on it by mistake.
	cwd := s.Cwd
	if !s.CwdIsLocal {
		cwd = ""
	}

	// Empty rather than null, so a script indexing into it needs no nil check.
	sessionTags := s.Tags
	if sessionTags == nil {
		sessionTags = map[string]string{}
	}

	return sessionJSON{
		Name:                s.Name,
		State:               stateName(s),
		ShellPID:            s.ShellPid,
		Clients:             int32(s.Clients),
		ExitCode:            s.ExitCode,
		Title:               s.Title,
		Cwd:                 cwd,
		CwdURI:              s.CwdUri,
		CwdIsLocal:          s.CwdIsLocal,
		CreatedAt:           created.Format(time.RFC3339),
		CreatedAtUnix:       s.CreatedAtUnix,
		Busy:                s.Busy,
		Command:             s.Command,
		LastCommandExitCode: s.LastCommandExitCode,
		CommandFinished:     s.CommandFinished,
		ReportedState:       s.ReportedState,
		ReportedDetail:      s.ReportedDetail,
		ReportedSource:      s.ReportedSource,
		Tags:                sessionTags,
	}
}

// truncate shortens s to at most n characters, marking that it was cut.
//
// For a table column only. The full value is in `cm info` and the JSON output, so nothing is lost by
// abbreviating here.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 3 {
		return s[:n]
	}
	return s[:n-3] + "..."
}

// firstWord returns the program name from a command line.
//
// The full command line can be arbitrarily long and would wreck the table, and the program is the part
// that identifies what is running at a glance. `cm info` and the JSON output carry the whole thing for
// anything that needs it.
func firstWord(cmd string) string {
	if i := strings.IndexAny(cmd, " \t"); i >= 0 {
		return cmd[:i]
	}
	return cmd
}

// stateName renders a session's lifecycle stage.
//
// Falls back to the older `exited` boolean when a server predates the state field, so a newer
// client against an older server reports something truthful rather than "unspecified".
func stateName(s *serverv1.Session) string {
	switch s.State {
	case serverv1.SessionState_SESSION_STATE_RUNNING:
		return "running"
	case serverv1.SessionState_SESSION_STATE_EXITED:
		return "exited"
	case serverv1.SessionState_SESSION_STATE_DEAD:
		return "dead"
	}
	if s.Exited {
		return "exited"
	}
	return "running"
}

// writeJSON encodes v as indented JSON with a trailing newline.
//
// Indented because this output is read by people as often as by scripts, and jq does not care
// either way.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// sortSessions orders sessions oldest first, so output is stable across calls.
func sortSessions(sessions []*serverv1.Session) {
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAtUnix != sessions[j].CreatedAtUnix {
			return sessions[i].CreatedAtUnix < sessions[j].CreatedAtUnix
		}
		// Names break ties, since sessions created in the same second would otherwise reorder
		// between calls.
		return sessions[i].Name < sessions[j].Name
	})
}

// printSessionsJSON writes a session list as JSON.
//
// Always an array, including when empty. A script doing `cm list --json | jq '.[]'` should not have
// to special-case "no sessions".
func printSessionsJSON(w io.Writer, sessions []*serverv1.Session) error {
	sortSessions(sessions)
	out := make([]sessionJSON, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, toSessionJSON(s))
	}
	return writeJSON(w, out)
}

// sessionStateColumn renders the STATE cell, which carries more than the lifecycle stage.
//
// What a session is *doing* is folded in here rather than given its own column, reusing the
// exited(0) idiom. That was a width decision and still is: CWD is a full path and sits last, so every
// column added before it costs a path its tail.
func sessionStateColumn(s *serverv1.Session) string {
	state := stateName(s)
	switch {
	case state == "exited":
		return fmt.Sprintf("exited(%d)", s.ExitCode)
	case state == "running" && s.ReportedState != "":
		// A report wins over the derived state, and its detail is the useful part: "blocked" alone
		// says a program wants something, while "blocked: needs approval" says what.
		out := "running(" + s.ReportedState
		if s.ReportedDetail != "" {
			// Truncated, not reduced to its first word: a detail is a sentence a human wrote, so
			// "needs approval" must not become "needs". A bound is still needed, since the column
			// has to stay readable and nothing stops a reporter sending a paragraph.
			out += ": " + truncate(s.ReportedDetail, 24)
		}
		return out + ")"
	case state == "running" && s.Command != "":
		return fmt.Sprintf("running(%s)", firstWord(s.Command))
	case state == "running" && s.Busy:
		// Busy but the shell did not say what. Distinguished from idle, since that is the part
		// that matters when deciding whether to close a window.
		return "running(busy)"
	}
	return state
}

// sessionCwdColumn renders the CWD cell, marking a directory that is not on this machine.
func sessionCwdColumn(s *serverv1.Session) string {
	cwd := s.Cwd
	if !s.CwdIsLocal && cwd != "" {
		// Marked rather than hidden: in a table the user wants to see that the session went
		// somewhere, which an empty column would not convey.
		cwd += " (remote)"
	}
	return cwd
}

// printSessionsTable writes a session list as aligned columns for a human.
//
// Columns are assembled from a list rather than written as format strings per combination. With TAGS
// and TITLE both optional, the branching version needed one Fprintf per combination and they had to
// agree on column order, which is exactly the shape that lets a header and its rows drift apart.
func printSessionsTable(w io.Writer, sessions []*serverv1.Session) error {
	if len(sessions) == 0 {
		return nil
	}
	sortSessions(sessions)

	// Optional columns appear only when some session has something to put in them.
	//
	// Width is the reason, and it is a real constraint rather than fussiness: CWD is a full path and
	// sits last, so each column before it eats into the path. Someone who tags nothing and whose shell
	// reports no title should pay for neither.
	//
	// Keyed on the whole list rather than per row, so the header and every row agree on the shape.
	// Deciding per row misaligns the table, which is worse than an empty cell.
	var showTags, showTitle bool
	for _, s := range sessions {
		if len(s.Tags) > 0 {
			showTags = true
		}
		if s.Title != "" {
			showTitle = true
		}
	}

	type column struct {
		header string
		cell   func(*serverv1.Session) string
	}
	columns := []column{
		{"NAME", func(s *serverv1.Session) string { return s.Name }},
		{"PID", func(s *serverv1.Session) string { return fmt.Sprint(s.ShellPid) }},
		{"CLIENTS", func(s *serverv1.Session) string { return fmt.Sprint(s.Clients) }},
		{"STATE", sessionStateColumn},
		{"CREATED", func(s *serverv1.Session) string {
			return humanAge(time.Unix(s.CreatedAtUnix, 0))
		}},
	}
	if showTitle {
		// A separate column rather than folded into STATE, because the two answer different
		// questions: the title says what a window *is* and the state says whether it is safe to
		// close. A terminal's own title is also the thing a person recognizes a window by, which is
		// what makes it the bridge between `cm list` and a tab in front of them.
		//
		// Before TAGS and CWD, since it is bounded in practice: a title is written to be read in a
		// tab, so it is already short by construction.
		columns = append(columns, column{"TITLE", func(s *serverv1.Session) string {
			// Bounded anyway. Nothing stops a program emitting a paragraph as its title, and the
			// full value is in `cm info` and the JSON output.
			return truncate(s.Title, 30)
		}})
	}
	if showTags {
		columns = append(columns, column{"TAGS", func(s *serverv1.Session) string {
			// Truncated for the same reason the reported detail is: the column has to stay readable,
			// and nothing bounds how many tags a session carries even though each one is bounded.
			return truncate(tags.Format(s.Tags), 32)
		}})
	}
	// CWD last, since a path is the one field that can be arbitrarily long and nothing after it
	// would line up on a normal terminal.
	columns = append(columns, column{"CWD", sessionCwdColumn})

	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	headers := make([]string, 0, len(columns))
	for _, c := range columns {
		headers = append(headers, c.header)
	}
	fmt.Fprintln(tw, strings.Join(headers, "\t"))

	for _, s := range sessions {
		cells := make([]string, 0, len(columns))
		for _, c := range columns {
			cells = append(cells, c.cell(s))
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

// humanAge renders an age compactly, since a full timestamp crowds out the columns that
// matter when scanning a session list.
func humanAge(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// printSessionInfo writes one session's details, or a single field.
//
// A single field prints bare, with no header or padding, because that is what a caller wants: a
// terminal emulator opening a new window in a session's directory should not have to parse
// anything.
// sessionFields returns the printable fields of a session, in display order.
//
// One list rather than a printer and a separate help string, because the two drifted: the flag's help named
// eight fields while the printer accepted sixteen, so busy, command, and the reported_* trio were usable and
// undocumented. Deriving the help from this makes that impossible.
func sessionFields(s *serverv1.Session) []struct {
	name  string
	value string
} {
	j := toSessionJSON(s)

	state := j.State
	if state == "exited" {
		state = fmt.Sprintf("exited(%d)", j.ExitCode)
	}

	return []struct {
		name  string
		value string
	}{
		{"name", j.Name},
		{"state", state},
		{"pid", fmt.Sprint(j.ShellPID)},
		{"clients", fmt.Sprint(j.Clients)},
		{"title", j.Title},
		{"cwd", j.Cwd},
		{"cwd_uri", j.CwdURI},
		{"cwd_is_local", fmt.Sprint(j.CwdIsLocal)},
		{"created_at", j.CreatedAt},
		{"busy", fmt.Sprint(j.Busy)},
		{"last_command_exit_code", fmt.Sprint(j.LastCommandExitCode)},
		{"command_finished", fmt.Sprint(j.CommandFinished)},
		{"command", j.Command},
		{"reported_state", j.ReportedState},
		{"reported_detail", j.ReportedDetail},
		{"reported_source", j.ReportedSource},
		// Rendered as "k=v,k" here rather than as a map, since --field prints one bare value for a
		// script to read. The JSON output carries the map for anything that wants the structure.
		{"tags", tags.Format(j.Tags)},
	}
}

// SessionFieldNames lists the fields `cm info --field` accepts, for the flag's help.
func SessionFieldNames() []string {
	fields := sessionFields(&serverv1.Session{})
	names := make([]string, 0, len(fields))
	for _, f := range fields {
		names = append(names, f.name)
	}
	return names
}

func printSessionInfo(w io.Writer, s *serverv1.Session, field string) error {
	fields := sessionFields(s)

	if field != "" {
		for _, f := range fields {
			if f.name == field {
				_, err := fmt.Fprintln(w, f.value)
				return err
			}
		}
		names := make([]string, 0, len(fields))
		for _, f := range fields {
			names = append(names, f.name)
		}
		return fmt.Errorf("unknown field %q, want one of %v", field, names)
	}

	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	for _, f := range fields {
		fmt.Fprintf(tw, "%s\t%s\n", f.name, f.value)
	}
	return tw.Flush()
}

// killJSON is the JSON shape of a kill result.
//
// Both outcomes are reported together rather than as a single success or failure, because killing
// several sessions can partly succeed and a caller needs to know which.
type killJSON struct {
	Killed []string `json:"killed"`
	// Errors maps a session name to why it could not be killed.
	Errors map[string]string `json:"errors"`
}

// reportKill writes a kill result, and returns an error when any session failed.
//
// The error is returned even in JSON mode, so a script can check the exit status rather than having
// to inspect the payload, while still getting the detail if it wants it.
func reportKill(w io.Writer, resp *serverv1.KillResponse, asJSON bool) error {
	out := killJSON{Killed: resp.Killed, Errors: resp.Errors}
	if out.Killed == nil {
		// An empty array rather than null, so a script can iterate unconditionally.
		out.Killed = []string{}
	}
	if out.Errors == nil {
		out.Errors = map[string]string{}
	}

	if asJSON {
		if err := writeJSON(w, out); err != nil {
			return err
		}
	} else {
		for _, name := range out.Killed {
			fmt.Fprintf(w, "killed %s\n", name)
		}
	}

	if len(out.Errors) == 0 {
		return nil
	}
	names := make([]string, 0, len(out.Errors))
	for name := range out.Errors {
		names = append(names, name)
	}
	sort.Strings(names)

	msgs := make([]string, 0, len(names))
	for _, name := range names {
		msgs = append(msgs, fmt.Sprintf("%s: %s", name, out.Errors[name]))
	}
	if asJSON {
		// The detail is already in the payload, so the error only has to set the exit status
		// without duplicating it onto stderr.
		return errAlreadyReported
	}
	return fmt.Errorf("%s", strings.Join(msgs, "; "))
}
