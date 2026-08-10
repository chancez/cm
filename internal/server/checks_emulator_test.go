package server

import (
	"strings"
	"testing"
	"time"
)

// slowScrollTerminal is a fakeTerminal that takes a fixed time on the scroll the probe measures,
// standing in for libghostty built in Debug.
//
// The delay is applied only to a write containing a reverse index, so the probe's fill stays fast and
// the measurement isolates the operation the real bug slowed down.
type slowScrollTerminal struct {
	fakeTerminal
	delay time.Duration
}

func (s *slowScrollTerminal) Write(p []byte) error {
	if strings.Contains(string(p), "\x1bM") {
		time.Sleep(s.delay)
	}
	return s.fakeTerminal.Write(p)
}

func TestCheckEmulatorSpeedReportsASlowEmulator(t *testing.T) {
	// Comfortably past the threshold, so this asserts on the check's logic rather than on how
	// precisely a sleep lands.
	slow := &slowScrollTerminal{delay: 4 * emulatorSlowThreshold}
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return slow, nil
	})

	findings := mgr.checkEmulatorSpeed()
	if len(findings) != 1 {
		t.Fatalf("checkEmulatorSpeed() returned %d findings, want 1: %+v", len(findings), findings)
	}

	got := findings[0]
	if got.Kind != FindingSlowEmulator {
		t.Errorf("finding kind = %q, want %q", got.Kind, FindingSlowEmulator)
	}
	if got.Fixable {
		t.Error("finding is marked fixable, want not fixable: rebuilding libghostty is not something " +
			"the server can do")
	}
	// The detail is what a reader acts on, so it has to name the cause rather than only the symptom.
	// A message that says "slow" without saying "rebuild libghostty" leaves the reader where they
	// started, which is the whole failure this check exists to prevent.
	for _, want := range []string{"libghostty", "-Doptimize", "mise run libghostty"} {
		if !strings.Contains(got.Detail, want) {
			t.Errorf("finding detail = %q, want it to mention %q", got.Detail, want)
		}
	}
}

func TestCheckEmulatorSpeedSilentOnAFastEmulator(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{}, nil
	})

	if findings := mgr.checkEmulatorSpeed(); len(findings) != 0 {
		t.Errorf("checkEmulatorSpeed() = %+v, want no findings for an emulator that is not slow",
			findings)
	}
}

// A build with no emulator is checkTerminal's finding to report, not this one. Reporting both would
// mean two findings for one condition, and noise is what teaches people to ignore diagnostics.
func TestCheckEmulatorSpeedSilentWithoutAnEmulator(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	if findings := mgr.checkEmulatorSpeed(); len(findings) != 0 {
		t.Errorf("checkEmulatorSpeed() = %+v, want no findings when the build has no emulator",
			findings)
	}
}

// The probe must exercise the operation that was slow. A probe that scrolled the wrong way, or that
// ran against an empty screen, would measure something cheap in both builds and report nothing while
// the bug was present, which is the failure mode that makes a check worse than none.
func TestCheckEmulatorSpeedProbesAScrollUp(t *testing.T) {
	term := &fakeTerminal{}
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})

	mgr.checkEmulatorSpeed()

	written := term.Written()
	// Home plus reverse index is what `less` emits per line when paging up, and the pairing is what
	// makes it a scroll rather than a cursor move.
	if !strings.Contains(written, "\x1b[H\x1bM") {
		t.Errorf("probe wrote %q, want it to contain a home followed by a reverse index",
			lastBytes(written, 64))
	}
	// And it must have filled the screen first, or there is nothing to scroll.
	if fill := strings.Count(written, "\r\n"); fill < emulatorProbeRows {
		t.Errorf("probe wrote %d lines of fill, want at least %d so rows have somewhere to shift from",
			fill, emulatorProbeRows)
	}
}

// lastBytes trims a value for an error message, so a failure prints the tail rather than a screenful.
func lastBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}
