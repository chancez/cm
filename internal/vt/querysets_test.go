package vt

import (
	"fmt"
	"strconv"
	"testing"

	"github.com/chancez/cm/internal/query"
)

// The two query sets must be disjoint, and together they must agree with the live emulator.
//
// This is the invariant the proxy design rests on, and both halves are load-bearing:
//
//   - **Disjoint.** A sequence in both sets would be answered by cm *and* proxied to a client, which is
//     the duplicate reply the design exists to make impossible. Nothing else checks this: each classifier
//     reads correctly on its own, and only comparing them catches an overlap.
//   - **Answerable matches the emulator.** A query the emulator answers but cm does not classify as
//     answerable gets proxied instead, so a client is asked a question its terminal will answer *and*
//     the emulator has already answered. A query cm classifies as answerable but the emulator ignores is
//     worse: nothing answers it and the asking program hangs.
//   - **Terminal-only excludes anything the emulator answers.** Same reasoning from the other side.
//
// Lives in internal/vt rather than internal/query because only this package can ask the real emulator. A
// hand-maintained list on the other side is what produced the last round of this bug: it agreed with
// itself perfectly while missing 28 sequences the emulator answered, including the one carrying the
// visible symptom.
func TestQuerySetsAgreeWithEmulator(t *testing.T) {
	// Swept rather than listed. The previous version of this work derived its classifier from a curated
	// table of 11 sequences and was wrong about 28 more, so the space is enumerated here instead: every
	// CSI final across the private markers and intermediates, the OSC query forms, the DCS requests, and
	// the single-character escapes.
	var seqs []string

	markers := []string{"", "?", ">", "=", "<"}
	inters := []string{"", "$", " "}
	params := []string{
		"", "0", "1", "2", "4", "5", "6", "11", "13", "14", "15", "16", "18", "19", "20", "21", "22",
		"23", "62", "996", "1000", "1004", "1049", "2004", "2026",
	}
	for _, m := range markers {
		for _, inter := range inters {
			for _, p := range params {
				for f := 0x40; f <= 0x7e; f++ {
					seqs = append(seqs, "\x1b["+m+p+inter+string(rune(f)))
				}
			}
		}
	}

	// OSC, in both the setter and query forms, since telling them apart is essential: OSC 11 with a
	// colour sets the background and must pass through, while OSC 11 with "?" asks for it.
	for n := 0; n <= 130; n++ {
		s := strconv.Itoa(n)
		seqs = append(seqs,
			"\x1b]"+s+";?\x07",
			"\x1b]"+s+";?\x1b\\",
			"\x1b]"+s+";rgb:1111/2222/3333\x07",
			"\x1b]"+s+";1;?\x07",
			"\x1b]"+s+";c;?\x07",
		)
	}
	for _, n := range []string{"777", "25453"} {
		seqs = append(seqs, "\x1b]"+n+";?\x07")
	}

	// DCS: XTGETTCAP (+q, terminal-only) and DECRQSS ($q, answerable).
	seqs = append(seqs,
		"\x1bP+q544e\x1b\\", "\x1bP+q6b75\x1b\\",
		"\x1bP$qm\x1b\\", "\x1bP$qr\x1b\\", "\x1bP$q q\x1b\\", "\x1bP$q\"p\x1b\\",
	)

	// Single-character escapes, and ordinary drawing output that must not be taken for a query.
	seqs = append(seqs, "\x1bZ", "\x1b7", "\x1b8", "\x1bc", "\x1b=", "\x1b>")

	var overlaps, missed, overreach, wrongProxy []string
	answeredCount, proxyCount := 0, 0

	for _, seq := range seqs {
		answerable := query.IsAnsweredRequest([]byte(seq))
		terminalOnly := query.IsTerminalOnlyRequest([]byte(seq))

		if answerable && terminalOnly {
			overlaps = append(overlaps, fmt.Sprintf("%q", seq))
		}
		if answerable {
			answeredCount++
		}
		if terminalOnly {
			proxyCount++
		}

		// What the emulator actually does with these bytes, asked rather than assumed.
		emulatorAnswers := len(emulatorReplies(t, seq)) > 0

		if emulatorAnswers && !answerable {
			missed = append(missed, fmt.Sprintf("%q", seq))
		}
		if !emulatorAnswers && answerable {
			overreach = append(overreach, fmt.Sprintf("%q", seq))
		}
		// A query cm intends to proxy must be one the emulator stays silent about.
		if emulatorAnswers && terminalOnly {
			wrongProxy = append(wrongProxy, fmt.Sprintf("%q", seq))
		}
	}

	t.Logf("swept %d sequences: %d answerable, %d terminal-only", len(seqs), answeredCount, proxyCount)

	for _, s := range overlaps {
		t.Errorf("OVERLAP: %s is classified both answerable and terminal-only.\n"+
			"cm would answer it from the model and also ask a client, so the shell receives two replies "+
			"to one question. The sets must be disjoint.", s)
	}
	for _, s := range missed {
		t.Errorf("MISSED: the emulator answers %s but it is not classified answerable.\n"+
			"It would be proxied to a client instead, so the client's terminal answers a question the "+
			"emulator has already answered.", s)
	}
	for _, s := range overreach {
		t.Errorf("OVERREACH: %s is classified answerable but the emulator does not answer it.\n"+
			"Nothing would answer it and the asking program hangs. This is the dangerous direction: fix "+
			"internal/query rather than this expectation.", s)
	}
	for _, s := range wrongProxy {
		t.Errorf("WRONG PROXY: %s is classified terminal-only but the emulator answers it.\n"+
			"cm would answer locally and still ask a client, producing a duplicate.", s)
	}

	// A floor rather than an exact number, so adding sweep cases does not require editing a constant,
	// but a classifier that silently stopped recognizing whole families still fails.
	if answeredCount < 100 {
		t.Errorf("only %d sequences classified answerable, want at least 100.\n"+
			"The emulator answers about 157 of the swept set; a number far below that means the "+
			"classifier has stopped recognizing a family.", answeredCount)
	}
	if proxyCount == 0 {
		t.Error("no sequences classified terminal-only, want the OSC colour, clipboard, XTGETTCAP, and " +
			"XTWINOPS pixel-size queries.\nWithout these the proxy path is dead code and OSC 11 hangs " +
			"as it did before.")
	}
}

// The specific queries the proxy exists for, named rather than left to the sweep.
//
// The sweep proves the sets are consistent with the emulator; it does not prove the *right* sequences are
// in the terminal-only set. These are the ones with recorded incidents behind them, so they are asserted
// by name: a refactor that quietly dropped OSC 11 would keep the sweep green and reintroduce the
// `wallfacer -h` hang.
func TestProxiedQueriesAreTheOnesThatMatter(t *testing.T) {
	proxied := []struct {
		name string
		seq  string
	}{
		// The recorded hang: wallfacer -h blocks reading the background colour.
		{"OSC 11 background query", "\x1b]11;?\x07"},
		{"OSC 11 background query, ST", "\x1b]11;?\x1b\\"},
		{"OSC 10 foreground query", "\x1b]10;?\x07"},
		{"OSC 12 cursor colour query", "\x1b]12;?\x07"},
		{"OSC 52 clipboard read", "\x1b]52;c;?\x07"},
		{"XTGETTCAP", "\x1bP+q544e\x1b\\"},
		{"XTWINOPS pixel size", "\x1b[14t"},
		{"XTWINOPS cell size", "\x1b[16t"},
		{"XTWINOPS text area", "\x1b[18t"},
	}
	for _, tc := range proxied {
		t.Run(tc.name, func(t *testing.T) {
			if !query.IsTerminalOnlyRequest([]byte(tc.seq)) {
				t.Errorf("%q is not classified terminal-only, want it proxied to a client.\n"+
					"cm cannot answer it, so without proxying the asking program waits forever. OSC 11 "+
					"is the recorded case: `wallfacer -h` blocks on it.", tc.seq)
			}
		})
	}

	// Setters must not be mistaken for queries. Proxying one would hold output behind a round trip that
	// answers nothing, and the value would never be applied.
	// OSC 4 is asserted separately: the emulator answers its query form, so it is answerable rather than
	// proxied. Named here because it was misclassified as terminal-only first, on the reasonable-sounding
	// but wrong theory that a palette belongs to the window.
	t.Run("OSC 4 palette query is answerable, not proxied", func(t *testing.T) {
		const seq = "\x1b]4;1;?\x07"
		if query.IsTerminalOnlyRequest([]byte(seq)) {
			t.Error("OSC 4 query is classified terminal-only, want answerable: libghostty models a " +
				"palette and answers from it, so proxying it would duplicate the reply")
		}
		if !query.IsAnsweredRequest([]byte(seq)) {
			t.Error("OSC 4 query is not classified answerable, want it to be")
		}
	})

	setters := []struct {
		name string
		seq  string
	}{
		{"OSC 11 set background", "\x1b]11;rgb:1111/2222/3333\x07"},
		{"OSC 4 set palette entry", "\x1b]4;1;rgb:1111/2222/3333\x07"},
		{"OSC 52 write clipboard", "\x1b]52;c;SGVsbG8=\x07"},
		{"OSC 2 set window title", "\x1b]2;a title\x07"},
		{"OSC 7 report cwd", "\x1b]7;file://host/tmp\x07"},
		{"XTWINOPS resize", "\x1b[8;24;80t"},
		{"cursor style", "\x1b[2 q"},
		{"SGR", "\x1b[1;32m"},
	}
	for _, tc := range setters {
		t.Run(tc.name, func(t *testing.T) {
			if query.IsTerminalOnlyRequest([]byte(tc.seq)) {
				t.Errorf("%q is classified terminal-only, want it treated as ordinary output.\n"+
					"It sets a value rather than asking for one, so proxying it stalls the reply queue "+
					"behind a question nobody asked.", tc.seq)
			}
			if query.IsAnsweredRequest([]byte(tc.seq)) {
				t.Errorf("%q is classified answerable, want it treated as ordinary output.", tc.seq)
			}
		})
	}
}
