package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"github.com/chancez/cm/internal/paths"
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
	// Name is what to call the session when talking to a person: one of its names, or "@" plus its ID
	// when nothing names it. Kept as "name" because that is what scripts already read.
	Name string `json:"name"`
	// ID is the session's identity, and what a script should record if it will come back later. A name
	// can be pointed at a different session; an ID cannot.
	ID string `json:"id"`
	// Names is every name bound to this session, oldest first, and empty for one nothing names.
	Names []string `json:"names"`
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
	// ReportedAt is when the report was made, RFC 3339, empty when nothing has reported.
	// ReportedAtUnix is the same instant in seconds, 0 when unknown.
	//
	// Worth reading rather than assuming a report is current: it is the program's last word and stands
	// until the program says otherwise, including across a server restart. A script deciding whether to
	// nudge a blocked session wants the age, not just the state.
	ReportedAt     string `json:"reported_at"`
	ReportedAtUnix int64  `json:"reported_at_unix"`
	// Tags are the caller's own labels for this session, set at creation or with `cm tag`.
	//
	// A map rather than the "k=v,k" string the table prints, since a script filtering on one key
	// should not have to parse a rendering meant for a column. Always present, and empty rather than
	// null for a session with no tags, so `jq '.[].tags.project'` does not have to special-case it.
	Tags map[string]string `json:"tags"`
	// Hosting names the sessions attached from inside this one by a nested `cm attach`.
	//
	// Also says why this session's own title and directory are standing still: while it hosts a nested
	// attach its shell is blocked inside `cm attach` and reports nothing, so those values are its last
	// true ones rather than stale ones. Empty rather than null for the usual case, like Tags.
	Hosting []string `json:"hosting"`
	// AttachedClients describes each client attached now, alongside the Clients count above.
	//
	// Added because diagnosing a lost session meant reconstructing what was attached from `ps` and
	// lsof, comparing binary inodes by hand to find which clients were running which build. Since a
	// shim outlives servers by design, a healthy install spans several builds at once: one incident
	// had twelve across twenty-six sessions. Empty rather than null, like Tags and Hosting.
	AttachedClients []attachedClientJSON `json:"attached_clients"`
}

// attachedClientJSON is the JSON shape of one attached client.
//
// Values a client reports about itself, so they are advisory: cm does not verify either, and a client
// older than this field sends neither. Both are reported as their zero value in that case rather than
// guessed at, which is why version is documented as possibly empty rather than as always present.
type attachedClientJSON struct {
	// PID is the client process, 0 when it did not report one. Meaningful only on this host, which is
	// where clients are.
	PID int32 `json:"pid"`
	// Version is the client's build, empty when it did not report one.
	//
	// A difference from the server's own build is legal rather than a fault: cm is built so a session
	// outlives its server, which means a client and server from different builds are expected. It is
	// worth seeing because the failure mode is silent, since protobuf reads a field a peer never sent
	// as its zero value rather than as an error.
	Version string `json:"version"`
	// ReadOnly reports a follower rather than an interactive terminal. A follower never sizes the
	// session and never answers a terminal query.
	ReadOnly bool `json:"read_only"`
	// AttachedAt is an RFC 3339 timestamp, empty when unknown. AttachedAtUnix is the same instant in
	// seconds, and 0 when unknown.
	AttachedAt     string `json:"attached_at"`
	AttachedAtUnix int64  `json:"attached_at_unix"`
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
	hosting := s.Hosting
	if hosting == nil {
		hosting = []string{}
	}

	// Empty rather than null, like the two above.
	clients := make([]attachedClientJSON, 0, len(s.AttachedClients))
	for _, c := range s.AttachedClients {
		ac := attachedClientJSON{
			PID:            c.Pid,
			Version:        c.Version,
			ReadOnly:       c.ReadOnly,
			AttachedAtUnix: c.AttachedAtUnix,
		}
		// Formatted only when there is a real instant. Rendering zero would print 1970, which reads as
		// a client attached decades ago rather than as one whose attach time is unknown.
		if c.AttachedAtUnix != 0 {
			ac.AttachedAt = time.Unix(c.AttachedAtUnix, 0).Format(time.RFC3339)
		}
		clients = append(clients, ac)
	}

	return sessionJSON{
		Name:                s.Name,
		ID:                  s.Id,
		Names:               s.Names,
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
		ReportedAt:          rfc3339OrEmpty(s.ReportedAtUnix),
		ReportedAtUnix:      s.ReportedAtUnix,
		Tags:                sessionTags,
		Hosting:             hosting,
		AttachedClients:     clients,
	}
}

// rfc3339OrEmpty renders a unix timestamp, keeping zero as empty rather than 1970.
//
// The same rule the attached-client times follow: an instant nobody recorded must not print as one
// decades ago, since a reader acts on the difference.
func rfc3339OrEmpty(unix int64) string {
	if unix == 0 {
		return ""
	}
	return time.Unix(unix, 0).Format(time.RFC3339)
}

// reportAgeThreshold is how old a report has to be before the STATE column shows its age.
//
// A threshold rather than always showing it, because the column is width-constrained and CWD sits last,
// so every character spent here costs a path its tail. Below it the age says nothing a reader does not
// assume: a program reports on each change, so a recent report is the current state. Above it the age is
// the interesting part, and it covers both cases that produce one -- a program blocked for hours, and a
// report that survived a server restart because cm now hands it over rather than forgetting it.
const reportAgeThreshold = time.Hour

// reportAge renders how long ago a report was made, empty when it is recent or unknown.
//
// now is a parameter so this is testable without a clock: the column it feeds is otherwise only
// checkable by waiting an hour.
//
// A report timestamped in the future reads as recent rather than as a negative age. That is a clock that
// moved, or a record written by a machine whose clock differs, and neither is worth putting in front of a
// user as "-3h".
func reportAge(unix int64, now time.Time) string {
	if unix == 0 {
		return ""
	}
	d := now.Sub(time.Unix(unix, 0))
	if d < reportAgeThreshold {
		return ""
	}
	if h := int(d.Hours()); h < 48 {
		return fmt.Sprintf("%dh", h)
	}
	return fmt.Sprintf("%dd", int(d.Hours())/24)
}

// truncate shortens s to at most n columns, marking that it was cut.
//
// For a table column only. The full value is in `cm info` and the JSON output, so nothing is lost by
// abbreviating here.
//
// Counts runes rather than bytes, for two reasons. Cutting at a byte offset splits a multibyte rune, and
// a title of accented characters came out as "ééé\xc3..." which a terminal paints as a replacement
// character: the abbreviation marker says the tail was dropped, so producing visible corruption next to
// it reads as cm mangling the title. And rune counting is what tabwriter itself does when it sizes a
// column, so a byte count would disagree with the aligner and make the width budget below wrong for any
// title that is not ASCII.
//
// Rune count is not display width: a CJK or emoji rune occupies two cells but counts as one here.
// Deliberate, because tabwriter counts it as one too, and matching the aligner keeps the columns lined up.
// Such a title overflows the budget by however many wide runes it holds, which costs alignment on that
// row rather than correctness.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 3 {
		return string(r[:n])
	}
	return string(r[:n-3]) + "..."
}

// displayWidth is the width tabwriter will assign a cell, in the units it counts.
func displayWidth(s string) int { return len([]rune(s)) }

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
		// Only when it is old enough to change what the state means. See reportAgeThreshold.
		if age := reportAge(s.ReportedAtUnix, time.Now()); age != "" {
			out += ", " + age
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

// shortenHome abbreviates a path under home to "~/...", the way a shell prompt does.
//
// Display only, and deliberately not applied to the JSON output or `cm info --field cwd`: those are read
// by scripts that cd into the value or hand it to a terminal emulator, and "~" only expands in a shell.
// The table is the one place the reader is a person, and the abbreviation buys back the width that
// matters most, since CWD sits last and every column before it eats into the path.
//
// Callers must pass a local path. A remote session's home is the remote user's, so rewriting it against
// this machine's would claim a directory relationship that does not exist.
//
// home is a parameter rather than looked up here so the behaviour is testable without setting HOME for
// the process.
func shortenHome(path, home string) string {
	// A trailing slash on HOME would otherwise make the prefix check below "//", so it is trimmed. Home
	// being empty or the root is not abbreviated at all: every absolute path is under "/", and rewriting
	// them all to "~" would hide the path rather than shorten it.
	home = strings.TrimSuffix(home, "/")
	if path == "" || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	// The separator is part of the prefix, so a sibling directory whose name merely starts with home's --
	// /home/user2 against /home/user -- is left alone rather than turned into "~2".
	if strings.HasPrefix(path, home+"/") {
		return "~" + path[len(home):]
	}
	return path
}

// sessionCwdColumn renders the CWD cell, marking a directory that is not on this machine.
func sessionCwdColumn(s *serverv1.Session) string {
	cwd := s.Cwd
	if s.CwdIsLocal {
		// An unresolvable home is not worth failing over: the column falls back to the full path, which
		// is what it printed before.
		if home, err := os.UserHomeDir(); err == nil {
			cwd = shortenHome(cwd, home)
		}
	}
	if !s.CwdIsLocal && cwd != "" {
		// Marked rather than hidden: in a table the user wants to see that the session went
		// somewhere, which an empty column would not convey.
		cwd += " (remote)"
	}
	// A session hosting a nested attach is annotated here rather than given its own column, for the
	// same reason "(remote)" is: it qualifies the directory shown beside it. This is the session whose
	// shell is sitting inside `cm attach`, so the path is where it was when it started attaching and
	// will not move until the nesting ends. Without the note that looks like a stale value, which is
	// what the underlying bug used to make it.
	//
	// Safe to append because CWD is the last column, so a variable-length addition cannot push
	// anything out of alignment.
	if len(s.Hosting) > 0 {
		note := "(hosting " + strings.Join(s.Hosting, " ") + ")"
		if cwd == "" {
			return note
		}
		return cwd + "  " + note
	}
	return cwd
}

// tablePadding is the cell padding handed to tabwriter, named because the width budget below has to do
// the same arithmetic the aligner does and a literal 2 in two places drifts.
const tablePadding = 2

// minTitleWidth is the narrowest the TITLE column is ever truncated to.
//
// Also the width the column had when it was fixed, which is deliberate: the dynamic budget only ever
// widens. Shrinking below this on a narrow terminal was considered and rejected, because the row is
// already too wide there for a reason TITLE cannot fix. CWD is last and unbounded, so an 80-column
// terminal showing a deep path wraps whatever TITLE does, and the trade would be a title cut shorter than
// today in exchange for a table that still does not fit.
const minTitleWidth = 30

// titleWidth returns the number of columns TITLE may occupy.
//
// termCols is the terminal's width, and 0 when output is not a terminal. reserved is what every other
// column costs, including padding. The remainder goes to TITLE, because it is the one column whose value
// is both frequently truncated and worth reading in full: a title is how a person recognizes which window
// a session is, so "claude: reviewing the wid..." identifies nothing.
//
// A separate function so the arithmetic is testable without a terminal. Getting it wrong does not fail,
// it wraps, and a wrapped row is the failure this is meant to avoid.
func titleWidth(termCols, reserved int) int {
	// Not a terminal: piped or redirected, where there is no width to fit and a value that changes with
	// the caller's window would make the output unreproducible. The fixed width is what it has always been.
	if termCols <= 0 {
		return minTitleWidth
	}
	if budget := termCols - reserved; budget > minTitleWidth {
		return budget
	}
	return minTitleWidth
}

// terminalWidth reports the width of w when it is a terminal, and 0 otherwise.
//
// Type-asserted rather than taking an *os.File, so the table printer keeps its io.Writer parameter and
// tests can pass a buffer, which reports 0 and takes the fixed width.
func terminalWidth(w io.Writer) int {
	f, ok := w.(*os.File)
	if !ok {
		return 0
	}
	cols, _, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0
	}
	return cols
}

// printSessionsTable writes a session list as aligned columns for a human.
//
// Columns are assembled from a list rather than written as format strings per combination. With TAGS
// and TITLE both optional, the branching version needed one Fprintf per combination and they had to
// agree on column order, which is exactly the shape that lets a header and its rows drift apart.
func printSessionsTable(w io.Writer, sessions []*serverv1.Session) error {
	return printSessionsTableWidth(w, sessions, terminalWidth(w))
}

// printSessionsTableWidth is printSessionsTable with the terminal width supplied, so a test can render
// against a width without owning a terminal of that size.
func printSessionsTableWidth(w io.Writer, sessions []*serverv1.Session, termCols int) error {
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
		// Always shown, unlike TAGS and TITLE below, and it costs eight columns of width to do it.
		//
		// Worth that: an ID is how a session is referred to when it matters that the reference cannot
		// be pointed elsewhere, and a named session's ID appears nowhere else, so leaving it out would
		// make the identity undiscoverable from the one command people run to find sessions. A session
		// with no names shows the same value in NAME, since that is what it is called.
		{"ID", func(s *serverv1.Session) string { return paths.FormatSessionID(s.Id) }},
		{"PID", func(s *serverv1.Session) string { return fmt.Sprint(s.ShellPid) }},
		{"CLIENTS", func(s *serverv1.Session) string { return fmt.Sprint(s.Clients) }},
		{"STATE", sessionStateColumn},
		{"CREATED", func(s *serverv1.Session) string {
			return humanAge(time.Unix(s.CreatedAtUnix, 0))
		}},
	}
	// titleIndex marks which column TITLE ended up at, so the sizing pass below can find it without
	// matching on the header string. -1 when no session reported a title and the column is absent.
	titleIndex := -1
	if showTitle {
		// A separate column rather than folded into STATE, because the two answer different
		// questions: the title says what a window *is* and the state says whether it is safe to
		// close. A terminal's own title is also the thing a person recognizes a window by, which is
		// what makes it the bridge between `cm list` and a tab in front of them.
		//
		// Before TAGS and CWD, since it is bounded in practice: a title is written to be read in a
		// tab, so it is already short by construction.
		//
		// Rendered whole here. The width it is allowed is not known until every other column has been
		// measured, so truncation happens in the sizing pass below rather than in this cell function.
		titleIndex = len(columns)
		columns = append(columns, column{"TITLE", func(s *serverv1.Session) string {
			return s.Title
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

	// Every cell is rendered before anything is written, because TITLE's width is the width left over
	// once the others are measured, and what they cost is only knowable from their content: PID is 5
	// digits or 6, and STATE is "running" or "running(blocked: needs approval)".
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		cells := make([]string, 0, len(columns))
		for _, c := range columns {
			cells = append(cells, c.cell(s))
		}
		rows = append(rows, cells)
	}

	if titleIndex >= 0 {
		// What the other columns cost, measured the way tabwriter measures: a column is as wide as its
		// widest cell, header included, plus the padding. The last column is not padded, since nothing
		// follows it to be separated from, which the probe confirmed rather than assumed.
		reserved := 0
		for i, c := range columns {
			if i == titleIndex {
				continue
			}
			width := displayWidth(c.header)
			for _, cells := range rows {
				if n := displayWidth(cells[i]); n > width {
					width = n
				}
			}
			if i != len(columns)-1 {
				width += tablePadding
			}
			reserved += width
		}
		// TITLE is padded too, since a column follows it. Counted against the budget rather than left
		// out, or the title would be exactly two columns too wide and wrap the row it was sized to fit.
		limit := titleWidth(termCols, reserved+tablePadding)
		for _, cells := range rows {
			cells[titleIndex] = truncate(cells[titleIndex], limit)
		}
	}

	tw := tabwriter.NewWriter(w, 0, 8, tablePadding, ' ', 0)
	headers := make([]string, 0, len(columns))
	for _, c := range columns {
		headers = append(headers, c.header)
	}
	fmt.Fprintln(tw, strings.Join(headers, "\t"))

	for _, cells := range rows {
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
		{"id", paths.FormatSessionID(j.ID)},
		// Every name, space separated, since a session can have several and `cm info` is where a person
		// goes to find out what a session is. Empty for one nothing names.
		{"names", strings.Join(j.Names, " ")},
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
		// When, so a caller can tell a program blocked right now from one that said so before the last
		// server restart. Empty when nothing has reported.
		{"reported_at", j.ReportedAt},
		// Rendered as "k=v,k" here rather than as a map, since --field prints one bare value for a
		// script to read. The JSON output carries the map for anything that wants the structure.
		{"tags", tags.Format(j.Tags)},
		// Space-separated, since a session name cannot contain a space and `--field hosting` is read
		// by a script that would otherwise have to strip punctuation.
		{"hosting", strings.Join(j.Hosting, " ")},
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
	// Surviving maps a session name to pids that outlived the signal.
	//
	// Separate from Errors because the kill did not fail: the session is gone and its record deleted.
	// What remains is a process holding a pty nothing will reattach to, which is a leak to warn about.
	// A caller that treats any survivor as failure can check this; one that does not is unaffected,
	// which is why it does not set the exit status.
	Surviving map[string][]int32 `json:"surviving"`
	// Unbound maps a name that was released to the session it had pointed at, for a name bound with
	// --borrow.
	//
	// Separate from Killed because the two are different outcomes: the session named here is still
	// running, and a teardown script that counted this as a kill would report work destroyed that is
	// still there, while one that treated it as a failure would retry something that already succeeded.
	Unbound map[string]string `json:"unbound"`
}

// plural renders a count with its unit, choosing the form rather than hedging with "(s)".
//
// Only correct for units that pluralize with a trailing s, which is every one it is used for.
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

// sortedKeys returns a map's keys in a stable order, so repeated output does not reshuffle.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// reportKill writes a kill result, and returns an error when any session failed.
//
// The error is returned even in JSON mode, so a script can check the exit status rather than having
// to inspect the payload, while still getting the detail if it wants it.
func reportKill(w io.Writer, resp *serverv1.KillResponse, asJSON bool) error {
	out := killJSON{
		Killed:    resp.Killed,
		Errors:    resp.Errors,
		Surviving: map[string][]int32{},
		Unbound:   resp.Unbound,
	}
	if out.Unbound == nil {
		out.Unbound = map[string]string{}
	}
	if out.Killed == nil {
		// An empty array rather than null, so a script can iterate unconditionally.
		out.Killed = []string{}
	}
	if out.Errors == nil {
		out.Errors = map[string]string{}
	}
	for name, sp := range resp.Surviving {
		out.Surviving[name] = sp.Pids
	}

	if asJSON {
		if err := writeJSON(w, out); err != nil {
			return err
		}
	} else {
		for _, name := range out.Killed {
			fmt.Fprintf(w, "killed %s\n", name)
		}
		// Said in full rather than as "unbound x", because the surprising half is what did *not* happen:
		// the caller asked to kill and the session is still running, which they have to know to decide
		// whether that was what they wanted.
		for _, name := range sortedKeys(out.Unbound) {
			fmt.Fprintf(w, "released %s, which named %s; that session is still running\n",
				name, paths.FormatSessionID(out.Unbound[name]))
		}
		// To stderr, and after the killed lines, so a script reading stdout for names is unaffected
		// while a person sees the warning next to what it refers to.
		//
		// Named as a leak rather than as a failure, and it names the fix: the pty a survivor holds is a
		// capped resource, and the symptom of exhausting it appears somewhere unrelated.
		for _, name := range sortedKeys(out.Surviving) {
			fmt.Fprintf(os.Stderr,
				"%s: warning: %s left process(es) %v running, still holding a pty; "+
					"retry with a stronger --signal\n",
				paths.Name, name, out.Surviving[name])
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
