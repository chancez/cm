//go:build cgo

package vt

import "testing"

// SizeReport must produce the sequence mode 2048 defines, byte for byte.
//
// Asserted against a literal rather than built from the same helper the code uses, so a change to the
// formatter has to be made deliberately in two places. A report with the fields transposed or a
// parameter missing is still a plausible-looking CSI, and a program reading it would take a size cm
// never meant: rows and cols reversed is a 100-row 30-column terminal.
//
// The pixel fields are 0 on purpose. cm has a grid in cells and no font, so it does not know how large a
// cell is, and 0 is what kitty itself sends when it cannot determine pixel size. Reporting a made-up
// number would be worse than reporting none.
func TestSizeReport(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rows, cols uint16
		want       string
	}{
		{name: "ordinary size", rows: 30, cols: 100, want: "\x1b[48;30;100;0;0t"},
		{name: "rows and cols are not interchangeable", rows: 100, cols: 30, want: "\x1b[48;100;30;0;0t"},
		{name: "single digits are not padded", rows: 5, cols: 9, want: "\x1b[48;5;9;0;0t"},
		// A zero size is not filtered here: the caller decides whether a resize is worth reporting, and
		// this records what the bytes are if one is asked for, rather than adding a second place that
		// silently declines.
		{name: "zero is formatted, not rejected", rows: 0, cols: 0, want: "\x1b[48;0;0;0;0t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(SizeReport(tc.rows, tc.cols)); got != tc.want {
				t.Errorf("SizeReport(%d, %d) = %q, want %q", tc.rows, tc.cols, got, tc.want)
			}
		})
	}
}

// The model must report whether the program asked to be told about resizes in band.
//
// This is what decides whether cm owes a report, and it is the piece that made the original bug
// invisible: nvim sends CSI ? 2048 h in its startup burst without waiting for the reply to the query it
// also sends, so cm answering "not recognized" does not stop the mode being set. Reading the model's
// actual state is the only way to know a report is owed. Measured by relaying nvim's own output: one
// query, one set, no reset.
//
// Driven through NewSessionTerminal with real bytes rather than by calling the mode accessor directly, so
// a version that reads the wrong mode number, or reads it from somewhere the program's DECSET does not
// reach, fails here.
func TestSessionTerminalTracksInBandResizeMode(t *testing.T) {
	st, err := NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer st.Close()

	// Nothing owed before the program asks. The negative case first, because a SizeReport that returned
	// a report unconditionally would pass every positive assertion below while putting bytes on the pty
	// of every session cm runs.
	report, err := st.SizeReport(30, 100)
	if err != nil {
		t.Fatalf("SizeReport() error = %v", err)
	}
	if report != nil {
		t.Errorf("SizeReport() = %q before the program enabled mode 2048, want nil.\n"+
			"An unrequested reply is echoed as text by a shell at a prompt.", report)
	}

	// The exact bytes nvim sends, rather than a constructed equivalent.
	if err := st.Write([]byte("\x1b[?2048h")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	report, err = st.SizeReport(30, 100)
	if err != nil {
		t.Fatalf("SizeReport() error = %v", err)
	}
	if want := "\x1b[48;30;100;0;0t"; string(report) != want {
		t.Errorf("SizeReport() = %q after DECSET 2048, want %q.\n"+
			"The program has stopped acting on SIGWINCH and is waiting for this, so no report means it "+
			"keeps drawing at its old size.", report, want)
	}

	// Resetting the mode stops the reports. The program is saying it will go back to SIGWINCH, and
	// continuing to send reports would put bytes on a pty nobody is reading them from.
	if err := st.Write([]byte("\x1b[?2048l")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	report, err = st.SizeReport(30, 100)
	if err != nil {
		t.Fatalf("SizeReport() error = %v", err)
	}
	if report != nil {
		t.Errorf("SizeReport() = %q after DECRST 2048, want nil", report)
	}
}

// A report cm sends must not be one cm strips.
//
// Two pieces of code know this sequence: SizeReport writes it and dropSizeReports recognizes the model's
// version of it. They are deliberately the same shape, because the drop exists to stop the model's
// untimely report while cm sends its own from the resize path. If they drift, cm either swallows its own
// reports or starts delivering the model's out of turn, and both failures are silent at the point of the
// mistake and only visible as a rendering bug much later.
//
// Kept at this level as well as in the server, because this is where both functions live and a change to
// either would be made here.
func TestDenyModesConsumesTheReportCmSends(t *testing.T) {
	report := SizeReport(24, 80)
	if got := DenyModes(report); len(got) != 0 {
		t.Errorf("DenyModes(SizeReport(24, 80)) = %q, want it consumed entirely", got)
	}
}
