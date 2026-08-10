package server

import (
	"fmt"
	"strings"
	"time"
)

// FindingSlowEmulator is a terminal emulator slow enough that scrolling is visibly delayed.
const FindingSlowEmulator = "slow-emulator"

// The measurement below is a scroll-up, which is the operation that exposed this.
//
// A reverse index with the cursor on the top row scrolls the screen, and libghostty built in Debug
// verifies the integrity of an entire page per row it shifts, each check standing up a fresh
// DebugAllocator. `less` emits home plus reverse index once per line when paging up, so the cost lands
// once per line rather than once per keypress.
const (
	// emulatorProbeRows and emulatorProbeCols are the size probed at.
	//
	// A realistic window rather than a minimal one, because the cost scales with cell count: the same
	// operation measured 1.8ms at 10x40 and 78ms at 100x200, so probing small would understate a real
	// terminal by an order of magnitude.
	emulatorProbeRows = 50
	emulatorProbeCols = 120
	// emulatorProbeLines is how many lines the probe scrolls, which is what `d` and `u` move.
	emulatorProbeLines = emulatorProbeRows / 2
	// emulatorSlowThreshold is when a half-page scroll is slow enough to see.
	//
	// Calibrated against both builds on real hardware rather than picked. A correct build takes about
	// 36us for this work and a Debug build about 350ms, a gap of four orders of magnitude, so anything
	// between them separates the two. 20ms is far above the noise a loaded machine adds to a
	// microsecond measurement and far below the broken case, and it is also roughly where a delay
	// starts being perceptible per keypress.
	emulatorSlowThreshold = 20 * time.Millisecond
)

// checkEmulatorSpeed reports a terminal emulator slow enough that scrolling a pager lags.
//
// This encodes a bug whose symptom pointed nowhere near its cause. Paging up in `git log` or `git diff`
// was visibly delayed while paging down was instant, which reads as a bug in the pager, in the shell, or
// in the terminal, since nothing about it suggests a multiplexer and nothing distinguishes the two
// directions from the outside.
//
// The cause was the libghostty build. Zig defaults to Debug and ghostty derives `slow_runtime_safety`
// from the optimize mode, so a build missing -Doptimize turned on integrity verification that walks a
// whole page per row shifted. A reverse index at the top row cost 14ms against 10us for the same
// sequence elsewhere. `less` emits one per line when scrolling up and plain lines when scrolling down,
// which is exactly why only one direction was slow: measured with a real `less` over a real pty, a
// keypress cost 145ms to 166ms of emulator time paging up against 1.2ms paging down.
//
// Measured rather than inferred from a build flag. The flag is not visible from Go, a future
// libghostty could regress this without the flag changing, and the thing worth reporting is the
// latency itself rather than how it was configured. The probe is what makes the check honest: it fires
// on the symptom, so it stays correct if the cause moves.
//
// Costs a few hundred microseconds on a correct build, which is why it runs unconditionally.
func (m *Manager) checkEmulatorSpeed() []Finding {
	if m.newTerminal == nil {
		// No emulator at all, which checkTerminal already reports. Saying it twice would be noise.
		return nil
	}

	term, err := m.newTerminal(emulatorProbeRows, emulatorProbeCols)
	if err != nil {
		// A factory that cannot produce a terminal is a real problem, but not this one, and it already
		// surfaces wherever a session needs a model. Reporting it here would attribute it to speed.
		return nil
	}
	defer term.Close()

	// Fill the screen and some scrollback, so the scroll below does the work a pager's would. An empty
	// terminal has nothing to shift, which is the state that would make this measure nothing.
	if err := term.Write([]byte(emulatorProbeFill())); err != nil {
		return nil
	}

	// Home the cursor before each reverse index, which is what makes it a scroll rather than a cursor
	// move, and is what `less` emits.
	scroll := strings.Repeat("\x1b[H\x1bM", emulatorProbeLines)

	start := time.Now()
	if err := term.Write([]byte(scroll)); err != nil {
		return nil
	}
	elapsed := time.Since(start)

	if elapsed < emulatorSlowThreshold {
		return nil
	}

	return []Finding{{
		Kind: FindingSlowEmulator,
		Detail: fmt.Sprintf(
			"the terminal emulator took %s to scroll half a %dx%d screen upward, against about 36us "+
				"expected, so scrolling up in a pager will lag while scrolling down stays fast; "+
				"libghostty is probably built without -Doptimize, and Zig's Debug default enables "+
				"integrity checks that verify a whole page per row moved. Rebuild it with "+
				"`mise run libghostty` and restart the server",
			elapsed.Round(time.Millisecond), emulatorProbeRows, emulatorProbeCols),
		Fixable: false,
	}}
}

// emulatorProbeFill returns enough output to fill the probe screen and some scrollback.
//
// Lines wide enough to be representative, since the per-row cost being measured depends on how many
// cells a row holds.
func emulatorProbeFill() string {
	var b strings.Builder
	line := strings.Repeat("x", emulatorProbeCols-1)
	// A screenful plus enough history that rows shift into scrollback, which is the state a pager sits
	// in when someone pages through a diff.
	for range emulatorProbeRows * 2 {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return b.String()
}
