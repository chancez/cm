// Package query classifies the terminal queries a program sends, so cm can answer the ones it knows
// and delegate the ones only a real terminal knows.
//
// The split this package exists to express, which is the whole design and is copied from tmux
// (`input.c`, see the memory note and docs/architecture.md):
//
//   - **Answerable** queries are ones a terminal *model* can answer: device attributes, device status,
//     cursor position, mode state, XTVERSION. cm answers these itself, always, attached or not.
//   - **Terminal-only** queries are ones no model can answer, because the value lives in the window:
//     the background and foreground colours, the palette, the clipboard, pixel dimensions, terminfo
//     capabilities. cm has to ask an attached client and relay what comes back.
//
// The important consequence is that a client is a *source cm consults*, never an answerer in its own
// right. Every reply reaching the pty is written by cm. That is what makes the duplicate-reply bug
// impossible by construction rather than by election: there is only ever one writer, so "two answerers
// on one pty" has no way to occur, and a query replayed to a client from the log cannot produce a
// second answer because clients do not answer.
//
// The previous design elected one attached client to answer directly and let its reply through. That is
// where every bug in this area came from. It needs the election to be right in four separate situations
// (nothing attached, one client, a read-only follower, several clients), it breaks when a client is
// reserved but not yet attached, and it breaks again across a server restart, when cm answers a query
// from the backlog and the reconnecting client answers the same query a moment later.
//
// Stateless across calls, which is a deliberate limitation with a recorded cost. A query split across
// two chunks is not recognized. Holding an incomplete trailing sequence back would trade a rare missed
// classification for delayed output on every chunk, which is felt as the session being slow. A shell
// writes a prompt hook's query in a single write, so the split is possible rather than routine.
package query

// Kind describes what cm can do about one terminal query.
type Kind int

const (
	// NotAQuery is ordinary output, or a sequence cm does not recognize as a query.
	NotAQuery Kind = iota
	// Answerable is a query cm's own terminal model answers.
	Answerable
	// TerminalOnly is a query only the real terminal can answer, so cm must ask a client.
	TerminalOnly
)

// Classify reports what the sequence at the start of p is, and how long it is.
//
// A length of zero means p does not begin a recognizable escape sequence, or begins one that is
// incomplete. The caller distinguishes those by position: at the end of a chunk it is a split sequence,
// anywhere else it is ordinary text.
func Classify(p []byte) (n int, kind Kind) {
	if n, answered := classify(p); answered {
		return n, Answerable
	}
	if n, terminal := classifyTerminalOnly(p); terminal {
		return n, TerminalOnly
	}
	// Length is still useful to a caller scanning a stream, so report what the answerable classifier
	// worked out about the sequence's extent even when it is not a query.
	n, _ = classify(p)
	return n, NotAQuery
}

// IsAnsweredRequest reports whether p is exactly one query cm's emulator answers.
//
// Kept as a single-sequence predicate for the cross-check against the live emulator, which asks about
// one sequence at a time. That test compares classifications rather than byte slices, so a failure names
// the sequence whose behavior moved.
func IsAnsweredRequest(p []byte) bool {
	n, answered := classify(p)
	return answered && n == len(p)
}

// IsTerminalOnlyRequest reports whether p is exactly one query only a real terminal can answer.
//
// The counterpart of IsAnsweredRequest, and pinned against the live emulator by the same test: a query
// in this set must be one the emulator stays silent about, or cm would both answer it and ask a client,
// which is the duplicate this design removes.
func IsTerminalOnlyRequest(p []byte) bool {
	n, terminal := classifyTerminalOnly(p)
	return terminal && n == len(p)
}

// classifyTerminalOnly recognizes the queries whose answer lives in the window rather than in a
// terminal model.
//
// Deliberately a short, enumerated list rather than "anything the emulator did not answer". An
// unrecognized sequence must not be treated as a query needing a round trip to a client: doing so would
// hold up the reply queue behind something that is not a question at all, and the timeout would be the
// only thing releasing it. So the default is NotAQuery, and this set grows only with sequences known to
// be real queries.
//
// Each entry is a query cm cannot answer for a structural reason, not merely one libghostty happens not
// to implement:
//
//   - OSC 10, 11, 12, 17, 19: foreground, background, cursor, and selection colours. Live in the
//     terminal's theme. `wallfacer -h` blocks on OSC 11, which is the recorded hang this set fixes.
//   - OSC 4: a palette entry, same reason.
//   - OSC 52 with a "?" payload: a clipboard read. Only the terminal has the clipboard.
//   - CSI 14/16/18 t: pixel size, cell size, and text-area size in pixels. cm knows its own grid in
//     cells but nothing about pixels, which depend on the font the client is rendering with.
//
// Note what is deliberately *absent*: CSI 21 t, the window title report. The emulator answers that one
// from the title it tracks, verified against the live emulator, so it belongs to the answerable set. A
// sequence must never be in both sets, or cm would answer it *and* ask a client, which is the exact
// duplicate this design exists to remove. The overlap check is asserted by a test rather than left to
// review.
//   - XTGETTCAP (DCS + q ... ST): terminfo capabilities of the real terminal.
func classifyTerminalOnly(p []byte) (n int, terminal bool) {
	if len(p) < 2 || p[0] != 0x1b {
		return 0, false
	}

	switch p[1] {
	case ']':
		return classifyOSCQuery(p)
	case 'P':
		// XTGETTCAP is DCS + q, distinguished from DECRQSS (DCS $ q), which the emulator answers.
		end, ok := dcsEnd(p)
		if !ok {
			return 0, false
		}
		if len(p) > 3 && p[2] == '+' && p[3] == 'q' {
			return end, true
		}
		return end, false
	case '[':
		return classifyCSITerminalOnly(p)
	}
	return 0, false
}

// classifyOSCQuery recognizes an OSC that asks for a value rather than setting one.
//
// The distinction is the "?" payload, and it is essential: OSC 11 with a colour *sets* the background
// and must pass through untouched, while OSC 11 with "?" asks for it and needs a client. Treating the
// setter as a query would hold output behind a round trip that answers nothing.
func classifyOSCQuery(p []byte) (n int, terminal bool) {
	end, body, ok := oscBody(p)
	if !ok {
		return 0, false
	}

	// Split the numeric prefix from the payload.
	i := 0
	for i < len(body) && body[i] >= '0' && body[i] <= '9' {
		i++
	}
	if i == 0 || i >= len(body) || body[i] != ';' {
		return end, false
	}
	num := string(body[:i])
	payload := body[i+1:]

	switch num {
	// OSC 4, the palette query, is deliberately absent: libghostty answers it from the palette it
	// models, verified by the sweep, so it belongs to the answerable set. tmux has the same shape here
	// (`colour_palette_get` replies directly and only proxies when the entry is unset), and listing it
	// as terminal-only was this implementation's first mistake, caught by the emulator cross-check.
	case "10", "11", "12", "17", "19":
		return end, isQueryPayload(payload)
	case "52":
		// OSC 52 is "clipboard;data", and a "?" in the data position is a read.
		if j := indexByte(payload, ';'); j >= 0 {
			return end, isQueryPayload(payload[j+1:])
		}
		return end, false
	}
	return end, false
}

// isQueryPayload reports whether an OSC payload is asking rather than setting.
func isQueryPayload(p []byte) bool {
	return len(p) == 1 && p[0] == '?'
}

func indexByte(p []byte, b byte) int {
	for i := range p {
		if p[i] == b {
			return i
		}
	}
	return -1
}

// classifyCSITerminalOnly recognizes the XTWINOPS reports that need the real window.
//
// Only the report forms, which are the ones with no intermediates and a private-marker-free parameter
// naming a report. The manipulation forms of XTWINOPS (move, resize, iconify) are not queries and must
// pass through.
func classifyCSITerminalOnly(p []byte) (n int, terminal bool) {
	params, inter, final, length, ok := parseCSI(p)
	if !ok {
		return 0, false
	}
	if final != 't' || len(inter) > 0 {
		return length, false
	}
	// A private marker means something else; the report forms are unmarked.
	if len(params) > 0 && !isDigit(params[0]) {
		return length, false
	}
	// The first parameter selects the operation.
	i := 0
	for i < len(params) && isDigit(params[i]) {
		i++
	}
	switch string(params[:i]) {
	case "14", "16", "18":
		return length, true
	}
	// 21 (the title report) is absent on purpose: the emulator answers it, so it is in the answerable
	// set. See the note on classifyTerminalOnly.
	return length, false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// classify consumes one escape sequence, reporting its length and whether the emulator answers it.
//
// A length of zero means p does not begin a recognizable sequence, or begins one that is incomplete.
// The caller distinguishes those two cases by position rather than here: at the end of a chunk it is
// a split sequence, and anywhere else it is ordinary text.
func classify(p []byte) (n int, answered bool) {
	if len(p) < 2 || p[0] != 0x1b {
		return 0, false
	}

	switch p[1] {
	case 'Z':
		// DECID, the historical spelling of a primary device attributes request.
		return 2, true
	case '[':
		return classifyCSI(p)
	case 'P':
		return classifyDCS(p)
	case ']':
		return classifyOSCAnswerable(p)
	}
	return 0, false
}

// classifyOSCAnswerable recognizes the one OSC query the emulator answers: OSC 4, a palette entry.
//
// Found by the emulator sweep rather than by reading libghostty. It was first classified as terminal-only
// on the reasoning that a palette belongs to the window, which is true of a real terminal and false of
// this emulator: libghostty models a palette and answers from it. tmux has the same shape, replying from
// `colour_palette_get` and proxying only when the entry is unset.
//
// The colour queries (OSC 10, 11, 12, 17, 19) are *not* here, because the emulator stays silent on those.
// That asymmetry is unintuitive enough to be worth stating: a palette index is modelled, the default
// foreground and background are not.
func classifyOSCAnswerable(p []byte) (n int, answered bool) {
	end, body, ok := oscBody(p)
	if !ok {
		return 0, false
	}
	// OSC 4 is "4;index;spec", and the query form has "?" as the spec.
	if len(body) < 2 || body[0] != '4' || body[1] != ';' {
		return end, false
	}
	rest := body[2:]
	j := indexByte(rest, ';')
	if j < 0 {
		return end, false
	}
	// The index must actually be a number. "4;c;?" parses structurally but names no palette entry, and
	// the emulator ignores it, so treating it as answerable would leave a query nothing replies to. Caught
	// by the sweep, which is the reason it enumerates malformed shapes alongside real ones.
	idx := rest[:j]
	if len(idx) == 0 {
		return end, false
	}
	for _, b := range idx {
		if !isDigit(b) {
			return end, false
		}
	}
	return end, isQueryPayload(rest[j+1:])
}

// classifyDCS handles the DCS requests, of which the emulator answers one family: DECRQSS.
//
// DCS $ q <name> ST asks the terminal to report a setting as a string, and libghostty answers the ones
// it models: SGR, the scrolling region, the cursor style, and the conformance level. XTGETTCAP
// (DCS + q ...) is the other DCS request and is *not* answered, since only the real terminal knows its
// terminfo, so the two are distinguished by the byte before the 'q' rather than treated alike.
//
// Missed on the first attempt at this package, because the curated list it was written against did not
// mention DECRQSS at all. A whole answered family was therefore left in the client log for a resuming
// client's terminal to answer a second time. Found by sweeping the emulator instead of trusting the
// list, which is why TestSweepEmulatorAnsweredQueries now exists.
func classifyDCS(p []byte) (n int, answered bool) {
	end, ok := dcsEnd(p)
	if !ok {
		// Unterminated at the end of a chunk. Reported incomplete rather than classified on a fragment.
		return 0, false
	}

	// DECRQSS carries "$q" immediately after the DCS introducer. Anything else, XTGETTCAP included, is
	// left alone: that one is answerable only by the real terminal and is classified there.
	if len(p) > 3 && p[2] == '$' && p[3] == 'q' {
		return end, true
	}
	return end, false
}

// classifyCSI handles the CSI forms, which is where every remaining answered query lives.
//
// Parsed structurally rather than matched against whole strings, because the parameter bytes vary: a
// terminal query carries an optional private-marker prefix and optional numeric parameters, and the
// forms that differ only by that prefix are answered differently. CSI 5 n is answered while
// CSI ? 996 n is not, and CSI ? 1049 $ p is answered while CSI 4 $ p is not, so a match that ignored
// the prefix would strip a query nobody answers and hang the program that sent it.
func classifyCSI(p []byte) (n int, answered bool) {
	params, inter, final, length, ok := parseCSI(p)
	if !ok {
		// Incomplete: the final byte has not arrived, or this is not a CSI sequence after all.
		return 0, false
	}

	// A private marker is the leading byte of the parameter string, and it is the whole reason this is
	// parsed rather than pattern-matched. Extracted once here so each case below can be read as the
	// query it names.
	var marker byte
	digits := params
	if len(params) > 0 && (params[0] == '<' || params[0] == '=' || params[0] == '>' || params[0] == '?') {
		marker, digits = params[0], params[1:]
	}

	switch final {
	case 'c':
		// Device attributes: primary with no marker, secondary with '>', tertiary with '='.
		//
		// The marked forms are matched whatever their parameters, which deliberately covers a DA2
		// *reply* as well as a DA2 request. The emulator answers its own reply, a fixpoint recorded by
		// TestEmulatorAnswersItsOwnReplies, and this set has to match what the emulator answers or the
		// cross-check fails. A reply reaches this path only when a shell echoes one at a prompt.
		if len(inter) > 0 {
			return length, false
		}
		switch marker {
		case 0:
			// DA1, whatever its parameter. The first version of this restricted it to a bare request and
			// an explicit zero, which is what the spec gives meaning to, and libghostty answers every
			// parameter value instead: CSI 1 c and CSI 62 c are answered as readily as CSI c. Matching the
			// spec rather than the emulator left those in the client log to be answered twice.
			return length, true
		case '>', '=':
			return length, true
		}
		return length, false
	case 't':
		// XTWINOPS. Mostly window manipulation, which the emulator does not answer since it has no
		// window, but CSI 21 t asks for the window *title* and libghostty answers that from the title it
		// tracks.
		//
		// This is the one that matters most in practice, and it was missing from the first version of
		// this package because the curated list did not include it. A title report is exactly what
		// carries a shell's "user@host: /path (branch)" text, so the duplicate reply that reached the
		// line editor after a restart was usually this sequence's. Sizes are asked with CSI 14 t and
		// CSI 18 t and are deliberately not stripped: only the real terminal knows them, so the emulator
		// stays silent and the query must reach a client.
		if marker != 0 || len(inter) > 0 {
			return length, false
		}
		return length, string(digits) == "21"
	case 'n':
		// Device status report. Only the unmarked forms are answered: the emulator answers 5n and 6n
		// but not the private CSI ? 996 n colour-scheme query, and stripping that one would hang
		// whatever asked it.
		if marker != 0 || len(inter) > 0 {
			return length, false
		}
		return length, string(digits) == "5" || string(digits) == "6"
	case 'q':
		// XTVERSION. The intermediate check is load-bearing: CSI 2 SP q sets the cursor style and must
		// pass through untouched.
		if len(inter) > 0 {
			return length, false
		}
		return length, marker == '>'
	case 'u':
		// The kitty keyboard protocol query, and equally its reply, which the emulator also answers.
		if len(inter) > 0 {
			return length, false
		}
		return length, marker == '?'
	case 'p':
		// DECRQM, asking whether a mode is set. Only the private form is answered; the ANSI form
		// (no marker) is not.
		//
		// A mode number is required. CSI ? $ p names no mode, so the emulator ignores it, and stripping
		// it was the one over-reach the sweep found. That direction is the dangerous one: the query is
		// removed from every client and nothing replies, so whatever sent it waits forever.
		return length, marker == '?' && string(inter) == "$" && len(digits) > 0
	}
	return length, false
}
