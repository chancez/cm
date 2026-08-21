package server

import (
	"strings"
	"testing"
)

// An emulator that reports the mode as merely reset is still a finding.
//
// ";2" is what cm actually answered, and it is the case a narrower check would miss: it reads as
// "the mode is off", which sounds harmless. nvim treats it as "the mode can be turned on" and uses
// margins, so reset and set do the same damage.
func TestCheckMarginModeReportsAResetMode(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{answers: map[string]string{marginModeQuery: "\x1b[?69;2$y"}}, nil
	})
	findings := mgr.checkMarginMode()
	if len(findings) != 1 {
		t.Fatalf("checkMarginMode() = %+v, want exactly one finding", findings)
	}
	got := findings[0]
	if got.Kind != FindingMarginModeClaimed {
		t.Errorf("finding kind = %q, want %q", got.Kind, FindingMarginModeClaimed)
	}
	if got.Fixable {
		t.Error("finding is marked fixable, want not fixable: the server cannot change what the " +
			"emulator answers at runtime")
	}
	// The detail has to carry the symptom, not just the mechanism, because the reader arrives having
	// seen the symptom and having no reason to suspect a capability reply.
	for _, want := range []string{"vertical split", "DECSLRM", "69"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("finding detail = %q, want it to mention %q", got.Detail, want)
		}
	}
}

// A mode reported as set is also a finding.
func TestCheckMarginModeReportsASetMode(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{answers: map[string]string{marginModeQuery: "\x1b[?69;1$y"}}, nil
	})

	if findings := mgr.checkMarginMode(); len(findings) != 1 {
		t.Errorf("checkMarginMode() = %+v, want one finding for a mode reported as set", findings)
	}
}

// The fixed behavior is silent. This is the check's control: without it, a check that reported
// unconditionally would pass every test above while being useless.
func TestCheckMarginModeSilentWhenDenied(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{answers: map[string]string{marginModeQuery: marginModeDenied}}, nil
	})

	if findings := mgr.checkMarginMode(); len(findings) != 0 {
		t.Errorf("checkMarginMode() = %+v, want no findings when the mode is reported as "+
			"not recognized", findings)
	}
}

// Permanently reset also tells a program the mode cannot be turned on, so it is accepted.
func TestCheckMarginModeSilentWhenPermanentlyReset(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{answers: map[string]string{marginModeQuery: "\x1b[?69;4$y"}}, nil
	})

	if findings := mgr.checkMarginMode(); len(findings) != 0 {
		t.Errorf("checkMarginMode() = %+v, want no findings when the mode is permanently reset",
			findings)
	}
}

// No reply at all is the honest state for a capability cm does not have.
func TestCheckMarginModeSilentWithNoReply(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{}, nil
	})

	if findings := mgr.checkMarginMode(); len(findings) != 0 {
		t.Errorf("checkMarginMode() = %+v, want no findings when nothing answers the query", findings)
	}
}

// A build with no emulator is checkTerminal's finding, not this one.
func TestCheckMarginModeSilentWithoutAnEmulator(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	if findings := mgr.checkMarginMode(); len(findings) != 0 {
		t.Errorf("checkMarginMode() = %+v, want no findings when the build has no emulator", findings)
	}
}

// The check must ask the question nvim asks. A probe sending a different sequence would measure
// nothing while the bug was present, which is the failure that makes a check worse than none.
func TestCheckMarginModeProbesDECRQM69(t *testing.T) {
	term := &fakeTerminal{}
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})

	mgr.checkMarginMode()

	if written := term.Written(); !strings.Contains(written, "\x1b[?69$p") {
		t.Errorf("probe wrote %q, want it to contain the DECRQM query for private mode 69", written)
	}
}
