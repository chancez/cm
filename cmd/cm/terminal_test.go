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
