package main

import (
	"testing"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/vt"
)

// A build without the emulator must hand the manager a nil factory rather than one that errors.
//
// The distinction is the whole point. The manager treats nil as "run without a terminal model" and
// keeps sessions, attach, detach, and persistence working, losing only screen restore and history.
// A factory that returns an error instead fails at session *creation*, so `cm run` and `cm attach`
// both die outright: the degradation stops being partial. That is what a CGO_ENABLED=0 binary
// actually did, and it looked like a broken build rather than a missing feature.
func TestTerminalFactoryNilWithoutCgo(t *testing.T) {
	cfg := &config.Config{}
	got := terminalFactory(cfg)

	if vt.Available {
		if got == nil {
			t.Fatal("terminalFactory() = nil with the emulator available, want a factory")
		}
		// And it produces a working terminal, otherwise "available" means nothing.
		term, err := got(24, 80)
		if err != nil {
			t.Fatalf("factory(24, 80) error = %v, want a terminal", err)
		}
		term.Close()
		return
	}

	if got != nil {
		t.Fatal("terminalFactory() returned a factory in a build without cgo, " +
			"want nil so the manager runs without a terminal model")
	}
}

// Expiry has to be configured even when persistence is disabled.
//
// These are separate concerns that shared one flag. persist.enabled decides whether a session *saves
// output*; every session that ends leaves a record either way. Gating the whole policy on the flag
// left the manager with no expiry at all, so a default install kept every finished session forever
// and `cm list` filled with every command ever run: 23 records from one test run, none of which held
// anything recoverable.
func TestPersistPolicyConfiguresExpiryWithPersistenceOff(t *testing.T) {
	cfg := &config.Config{}
	if cfg.Persist.Enabled {
		t.Fatal("the zero config has persistence enabled, want it off for this test")
	}

	policy, err := persistPolicy(cfg)
	if err != nil {
		t.Fatalf("persistPolicy() error = %v", err)
	}

	// Expiry intervals must be set, or ExpireDeadSessions returns early and nothing is cleaned up.
	if policy.ExpireAfter <= 0 {
		t.Errorf("ExpireAfter = %v, want a positive default", policy.ExpireAfter)
	}
	if policy.ForgetUnpersistedAfter <= 0 {
		t.Errorf("ForgetUnpersistedAfter = %v, want a positive default", policy.ForgetUnpersistedAfter)
	}
	// A record with no saved output must be forgotten sooner than a persisted one, or short commands
	// linger for the persisted lifetime.
	if policy.ForgetUnpersistedAfter >= policy.ExpireAfter {
		t.Errorf("ForgetUnpersistedAfter = %v, want less than ExpireAfter = %v",
			policy.ForgetUnpersistedAfter, policy.ExpireAfter)
	}

	// And nothing is persisted, since that is what the flag actually controls.
	if policy.Matches("anything") {
		t.Error("Matches() = true with persistence disabled, want nothing persisted")
	}
}
