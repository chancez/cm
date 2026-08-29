//go:build cm_testhooks

package fault

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Parsing is tested rather than assumed because a spec that cannot be parsed injects nothing, and a test
// built on one passes while proving nothing. That is the exact failure this package exists to prevent, so
// it would be a poor thing for the package itself to have.
func TestParse(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entry   string
		wantErr bool
		point   Point
		kind    kind
		dur     time.Duration
		path    string
		count   int64
	}{
		{
			name:  "a delay",
			entry: "after-log-append:delay=50ms",
			point: AfterLogAppend, kind: kindDelay, dur: 50 * time.Millisecond, count: -1,
		},
		{
			name:  "a pause",
			entry: "before-model-feed:pause=/tmp/go",
			point: BeforeModelFeed, kind: kindPause, path: "/tmp/go", count: -1,
		},
		{
			name:  "an error",
			entry: "before-shim-write:error",
			point: BeforeShimWrite, kind: kindError, count: -1,
		},
		{
			name:  "a bounded count",
			entry: "before-shim-write:error:count=2",
			point: BeforeShimWrite, kind: kindError, count: 2,
		},
		{name: "an unknown point", entry: "no-such-point:delay=1s", wantErr: true},
		{name: "an unknown type", entry: "after-log-append:explode", wantErr: true},
		{name: "a delay with no duration", entry: "after-log-append:delay", wantErr: true},
		{name: "a delay of zero", entry: "after-log-append:delay=0s", wantErr: true},
		{name: "a pause with no path", entry: "after-log-append:pause", wantErr: true},
		{name: "an unknown option", entry: "after-log-append:delay=1s:twice", wantErr: true},
		{name: "a count of zero", entry: "after-log-append:delay=1s:count=0", wantErr: true},
		{name: "no type at all", entry: "after-log-append", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, p, err := parse(tc.entry)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parse(%q) succeeded, want an error: a spec that cannot be used must be "+
						"reported, not silently ignored", tc.entry)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse(%q) error = %v", tc.entry, err)
			}
			// The whole parsed value, not one field at a time, so a field the parser sets wrongly cannot
			// hide behind the one the test happens to check.
			got := struct {
				Point Point
				Kind  kind
				Dur   time.Duration
				Path  string
				Count int64
			}{p, f.kind, f.dur, f.path, f.remaining.Load()}
			want := struct {
				Point Point
				Kind  kind
				Dur   time.Duration
				Path  string
				Count int64
			}{tc.point, tc.kind, tc.dur, tc.path, tc.count}
			if got != want {
				t.Errorf("parse(%q) = %+v, want %+v", tc.entry, got, want)
			}
		})
	}
}

// A count bounds how many times a fault fires, which is what makes "fail the first write and then work"
// expressible. Without it a test can only choose between never and always.
func TestCountLimitsHowOftenAFaultFires(t *testing.T) {
	f, _, err := parse("before-shim-write:error:count=2")
	if err != nil {
		t.Fatal(err)
	}
	got := [4]bool{f.take(), f.take(), f.take(), f.take()}
	want := [4]bool{true, true, false, false}
	if got != want {
		t.Errorf("take() over four calls = %v, want %v", got, want)
	}
}

// No count means every time, which is the default because widening a window is the common use and a window
// that only widens once is a window a test still has to race for.
func TestNoCountMeansEveryTime(t *testing.T) {
	f, _, err := parse("after-log-append:delay=1ms")
	if err != nil {
		t.Fatal(err)
	}
	for i := range 5 {
		if !f.take() {
			t.Fatalf("take() was false on call %d, want a fault with no count to fire every time", i)
		}
	}
}

// Err returns the sentinel so a test can tell an injected failure from a real one, and the point's name so
// a log says which.
func TestErrReportsTheInjectedSentinel(t *testing.T) {
	t.Setenv("CM_TESTHOOK_FAULTS", "before-shim-write:error")
	// Reset the once-only parse, since another test in this package may have triggered it.
	resetForTest()

	err := Err(BeforeShimWrite)
	if !errors.Is(err, ErrInjected) {
		t.Fatalf("Err() = %v, want it to wrap ErrInjected", err)
	}
	if got := err.Error(); !contains(got, string(BeforeShimWrite)) {
		t.Errorf("Err() = %q, want it to name the point so a log says which one fired", got)
	}

	// A point with nothing configured must be free and silent, or every call site would have to care.
	if err := Err(AfterLogAppend); err != nil {
		t.Errorf("Err() at an unconfigured point = %v, want nil", err)
	}
}

// A pause releases when the file appears, which is how a test that has to act inside a window coordinates
// with a server it spawned.
func TestPauseReleasesWhenTheFileAppears(t *testing.T) {
	release := filepath.Join(t.TempDir(), "go")
	t.Setenv("CM_TESTHOOK_FAULTS", "after-log-append:pause="+release)
	resetForTest()

	done := make(chan struct{})
	go func() {
		At(AfterLogAppend)
		close(done)
	}()

	// Still held while the file does not exist. Asserted rather than assumed, or the test would pass
	// against a pause that never paused.
	select {
	case <-done:
		t.Fatal("At() returned before the release file existed, so the pause did not hold")
	case <-time.After(50 * time.Millisecond):
	}

	if err := touch(release); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("At() did not return after the release file appeared")
	}
}

// An unparseable spec leaves everything unconfigured rather than half-configured, and the entries around a
// bad one still apply: a typo in one place must not silently disable the fault a test is relying on
// elsewhere.
func TestABadEntryDoesNotDisableTheGoodOnes(t *testing.T) {
	t.Setenv("CM_TESTHOOK_FAULTS", "no-such-point:delay=1s,before-shim-write:error")
	resetForTest()

	if err := Err(BeforeShimWrite); !errors.Is(err, ErrInjected) {
		t.Errorf("Err() = %v, want the good entry to still apply alongside a rejected one", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
