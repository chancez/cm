package query

import "testing"

// The text area in cells is cm's own answer, not a question for a terminal.
//
// Proxying it cost a measured 500ms stall on a session with no client, and returned no answer at all,
// for a value cm set on the pty itself. The classification is what makes that stop happening, so it is
// asserted directly rather than inferred from behaviour further up.
func TestTheTextAreaReportIsNotProxied(t *testing.T) {
	const seq = "\x1b[18t"
	if IsTerminalOnlyRequest([]byte(seq)) {
		t.Errorf("IsTerminalOnlyRequest(%q) = true, want false: asking a client for the size cm set is "+
			"what stalls every reply behind it for the full timeout", seq)
	}
	if !IsLocalReport([]byte(seq)) {
		t.Errorf("IsLocalReport(%q) = false, want true", seq)
	}
	if IsAnsweredRequest([]byte(seq)) {
		t.Errorf("IsAnsweredRequest(%q) = true, want false: the emulator is silent about this one, and "+
			"TestQuerySets checks that against the live emulator", seq)
	}
	if n, kind := Classify([]byte(seq)); n != len(seq) || kind != LocalReport {
		t.Errorf("Classify(%q) = (%d, %v), want (%d, LocalReport)", seq, n, kind, len(seq))
	}
}

// The pixel reports stay proxied, because a real terminal's answer is better than cm's derived one.
//
// A font change moves the cell size and cm never learns of it, so these are a fallback rather than a
// replacement. Asserted because moving them out of the proxied set is the tempting next step and would
// serve a stale cell size to an attached terminal that could have answered correctly.
func TestThePixelReportsStayProxied(t *testing.T) {
	for _, seq := range []string{"\x1b[14t", "\x1b[16t"} {
		if !IsTerminalOnlyRequest([]byte(seq)) {
			t.Errorf("IsTerminalOnlyRequest(%q) = false, want true", seq)
		}
		if IsLocalReport([]byte(seq)) {
			t.Errorf("IsLocalReport(%q) = true, want false: a client answers this better when attached", seq)
		}
	}
}

func TestReport(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		seq                    string
		rows, cols, xpix, ypix uint16
		want                   string
		wantOK                 bool
	}{
		{
			name: "text area in cells", seq: "\x1b[18t",
			rows: 24, cols: 80, xpix: 800, ypix: 600,
			want: "\x1b[8;24;80t", wantOK: true,
		},
		{
			// Cells alone are enough: a pty with no pixel dimensions still has a size in characters.
			name: "text area in cells without pixels", seq: "\x1b[18t",
			rows: 24, cols: 80,
			want: "\x1b[8;24;80t", wantOK: true,
		},
		{
			name: "text area in pixels", seq: "\x1b[14t",
			rows: 24, cols: 80, xpix: 800, ypix: 600,
			want: "\x1b[4;600;800t", wantOK: true,
		},
		{
			name: "cell size in pixels", seq: "\x1b[16t",
			rows: 24, cols: 80, xpix: 800, ypix: 600,
			want: "\x1b[6;25;10t", wantOK: true,
		},
		{
			// Silence rather than zero. Zero is what kitten icat reads as "cannot report pixel sizes",
			// and it then refuses to transmit an image at all rather than falling back.
			name: "pixels unknown", seq: "\x1b[14t",
			rows: 24, cols: 80,
			wantOK: false,
		},
		{
			name: "not a report", seq: "\x1b[c",
			rows: 24, cols: 80, xpix: 800, ypix: 600,
			wantOK: false,
		},
		{
			// The manipulation forms of XTWINOPS are not queries and must not be answered.
			name: "a resize request is not a report", seq: "\x1b[8;24;80t",
			rows: 24, cols: 80, xpix: 800, ypix: 600,
			wantOK: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := Report([]byte(tc.seq), tc.rows, tc.cols, tc.xpix, tc.ypix)
			if ok != tc.wantOK || string(got) != tc.want {
				t.Errorf("Report(%q, %d, %d, %d, %d) = (%q, %v), want (%q, %v)",
					tc.seq, tc.rows, tc.cols, tc.xpix, tc.ypix, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
