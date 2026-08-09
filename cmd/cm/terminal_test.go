package main

import (
	"testing"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/vt"
)

// terminalFactory must return a working factory, since cgo is required.
//
// This used to assert the opposite branch too: that a build without the emulator handed the manager a *nil*
// factory rather than one that errors, so sessions kept working and only screen restore was lost. That branch
// is gone with the no-cgo build, which was retired because the degraded mode was not worth having -- `cm read`,
// `cm history`, and restore are most of what cm does, and a build where they quietly return nothing is worse
// than one that does not compile.
//
// What is left is worth keeping: the factory has to produce a terminal, or "the emulator is available" means
// nothing.
func TestTerminalFactoryProducesATerminal(t *testing.T) {
	cfg := &config.Config{}
	got := terminalFactory(cfg)
	if got == nil {
		t.Fatal("terminalFactory() = nil, want a factory now that cgo is required")
	}
	if !vt.Available {
		t.Fatal("vt.Available = false, want true now that cgo is required")
	}

	term, err := got(24, 80)
	if err != nil {
		t.Fatalf("factory(24, 80) error = %v, want a terminal", err)
	}
	term.Close()
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
