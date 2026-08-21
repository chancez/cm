package server

import (
	"fmt"
	"strings"
	"testing"
)

// query is the DECRQM probe for a mode, and denied is the reply that suppresses the behavior.
func modeQuery(mode int) string  { return fmt.Sprintf("\x1b[?%d$p", mode) }
func modeDenied(mode int) string { return fmt.Sprintf("\x1b[?%d;0$y", mode) }

// An emulator that reports a denied mode as merely reset is still a finding.
//
// ";2" is what cm actually answered for both modes, and it is the case a narrower check would miss: it
// reads as "the mode is off", which sounds harmless. nvim treats it as "the mode can be turned on" and
// takes the path anyway, so reset and set do the same damage.
//
// Each mode carries its own symptom text, and the assertion checks for it specifically. A check that
// reported the right count with the wrong prose would leave the reader no better off, since the whole
// value here is naming a symptom that points elsewhere.
func TestCheckDeniedModesReportsAResetMode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mode     int
		mentions []string
	}{
		{name: "left/right margins", mode: 69, mentions: []string{"vertical split", "DECSLRM", "69"}},
		{name: "in-band size reports", mode: 2048, mentions: []string{"old height", "SIGWINCH", "2048"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
				return &fakeTerminal{answers: map[string]string{
					modeQuery(tc.mode): fmt.Sprintf("\x1b[?%d;2$y", tc.mode),
				}}, nil
			})

			findings := mgr.checkDeniedModes()
			if len(findings) != 1 {
				t.Fatalf("checkDeniedModes() = %+v, want exactly one finding", findings)
			}
			got := findings[0]
			if got.Kind != FindingModeClaimed {
				t.Errorf("finding kind = %q, want %q", got.Kind, FindingModeClaimed)
			}
			if got.Fixable {
				t.Error("finding is marked fixable, want not fixable: the server cannot change what " +
					"the emulator answers at runtime")
			}
			// The detail has to carry the symptom, not just the mechanism, because the reader arrives
			// having seen the symptom and having no reason to suspect a capability reply.
			for _, want := range tc.mentions {
				if !strings.Contains(got.Detail, want) {
					t.Errorf("finding detail = %q, want it to mention %q", got.Detail, want)
				}
			}
		})
	}
}

// A mode reported as set is also a finding.
func TestCheckDeniedModesReportsASetMode(t *testing.T) {
	for _, mode := range []int{69, 2048} {
		t.Run(fmt.Sprint(mode), func(t *testing.T) {
			mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
				return &fakeTerminal{answers: map[string]string{
					modeQuery(mode): fmt.Sprintf("\x1b[?%d;1$y", mode),
				}}, nil
			})

			if findings := mgr.checkDeniedModes(); len(findings) != 1 {
				t.Errorf("checkDeniedModes() = %+v, want one finding for a mode reported as set",
					findings)
			}
		})
	}
}

// Every denied mode is probed, so one claimed mode is found even when the other is correctly denied.
//
// The regression this guards is a check that stopped at the first mode, which would report nothing once
// mode 69 was fixed and leave 2048 unwatched. That is the shape the original single-mode check had.
func TestCheckDeniedModesFindsEachModeIndependently(t *testing.T) {
	for _, claimed := range []int{69, 2048} {
		t.Run(fmt.Sprint(claimed), func(t *testing.T) {
			mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
				// Every mode answers "not recognized" except the one under test.
				answers := map[string]string{}
				for _, check := range deniedModeChecks {
					answers[modeQuery(check.mode)] = modeDenied(check.mode)
				}
				answers[modeQuery(claimed)] = fmt.Sprintf("\x1b[?%d;2$y", claimed)
				return &fakeTerminal{answers: answers}, nil
			})

			findings := mgr.checkDeniedModes()
			if len(findings) != 1 {
				t.Fatalf("checkDeniedModes() = %+v, want exactly one finding for mode %d alone",
					findings, claimed)
			}
			if want := fmt.Sprint(claimed); !strings.Contains(findings[0].Detail, want) {
				t.Errorf("finding detail = %q, want it to name mode %d", findings[0].Detail, claimed)
			}
		})
	}
}

// Both modes claimed produces a finding for each, rather than one that mentions the first.
func TestCheckDeniedModesReportsEveryClaimedMode(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		answers := map[string]string{}
		for _, check := range deniedModeChecks {
			answers[modeQuery(check.mode)] = fmt.Sprintf("\x1b[?%d;2$y", check.mode)
		}
		return &fakeTerminal{answers: answers}, nil
	})

	findings := mgr.checkDeniedModes()
	if len(findings) != len(deniedModeChecks) {
		t.Fatalf("checkDeniedModes() = %+v, want %d findings", findings, len(deniedModeChecks))
	}
}

// The fixed behavior is silent. This is the check's control: without it, a check that reported
// unconditionally would pass every test above while being useless.
func TestCheckDeniedModesSilentWhenDenied(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		answers := map[string]string{}
		for _, check := range deniedModeChecks {
			answers[modeQuery(check.mode)] = modeDenied(check.mode)
		}
		return &fakeTerminal{answers: answers}, nil
	})

	if findings := mgr.checkDeniedModes(); len(findings) != 0 {
		t.Errorf("checkDeniedModes() = %+v, want no findings when every mode is reported as "+
			"not recognized", findings)
	}
}

// Permanently reset also tells a program the mode cannot be turned on, so it is accepted.
func TestCheckDeniedModesSilentWhenPermanentlyReset(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		answers := map[string]string{}
		for _, check := range deniedModeChecks {
			answers[modeQuery(check.mode)] = fmt.Sprintf("\x1b[?%d;4$y", check.mode)
		}
		return &fakeTerminal{answers: answers}, nil
	})

	if findings := mgr.checkDeniedModes(); len(findings) != 0 {
		t.Errorf("checkDeniedModes() = %+v, want no findings when the modes are permanently reset",
			findings)
	}
}

// No reply at all is the honest state for a capability cm does not have.
func TestCheckDeniedModesSilentWithNoReply(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return &fakeTerminal{}, nil
	})

	if findings := mgr.checkDeniedModes(); len(findings) != 0 {
		t.Errorf("checkDeniedModes() = %+v, want no findings when nothing answers the query", findings)
	}
}

// A build with no emulator is checkTerminal's finding, not this one.
func TestCheckDeniedModesSilentWithoutAnEmulator(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)

	if findings := mgr.checkDeniedModes(); len(findings) != 0 {
		t.Errorf("checkDeniedModes() = %+v, want no findings when the build has no emulator", findings)
	}
}

// The check must ask the questions nvim asks. A probe sending a different sequence would measure
// nothing while the bug was present, which is the failure that makes a check worse than none.
func TestCheckDeniedModesProbesEachMode(t *testing.T) {
	var terms []*fakeTerminal
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		term := &fakeTerminal{}
		terms = append(terms, term)
		return term, nil
	})

	mgr.checkDeniedModes()

	var written strings.Builder
	for _, term := range terms {
		written.WriteString(term.Written())
	}
	for _, check := range deniedModeChecks {
		if want := modeQuery(check.mode); !strings.Contains(written.String(), want) {
			t.Errorf("probes wrote %q, want the DECRQM query %q for private mode %d",
				written.String(), want, check.mode)
		}
	}
}

// A model claiming a mode whose number merely contains a denied one is not a finding.
//
// The reply is matched on the full "CSI ? Pm ;" prefix rather than on the digits alone, so mode 12048
// reporting "supported" must not be read as mode 2048 doing so. Without this, a check could fire on a
// mode cm has no opinion about.
func TestCheckDeniedModesIgnoresASimilarModeNumber(t *testing.T) {
	mgr, _, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		answers := map[string]string{}
		for _, check := range deniedModeChecks {
			// The query for each denied mode is answered about a *different*, longer mode number.
			answers[modeQuery(check.mode)] = fmt.Sprintf("\x1b[?1%d;2$y", check.mode)
		}
		return &fakeTerminal{answers: answers}, nil
	})

	if findings := mgr.checkDeniedModes(); len(findings) != 0 {
		t.Errorf("checkDeniedModes() = %+v, want no findings when only a similar mode number is "+
			"claimed", findings)
	}
}
