//go:build !race

package e2e

// raceEnabled reports whether this test binary was built with -race. See race_on_test.go.
const raceEnabled = false
