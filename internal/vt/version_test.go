package vt

import (
	"os"
	"strings"
	"testing"
)

// The commit scripts/libghostty.sh stamps into the library reaches what cm reports. Skipped outside mise,
// where GHOSTTY_REF is unset and the library may have been built some other way.
func TestLibraryVersionNamesThePinnedCommit(t *testing.T) {
	ref := os.Getenv("GHOSTTY_REF")
	if len(ref) < 12 {
		t.Skip("GHOSTTY_REF is not set, so the linked library's build is unknown")
	}
	got := LibraryVersion()
	want := "+" + ref[:12]
	if !strings.HasSuffix(got, want) {
		t.Errorf("LibraryVersion() = %q, want it to end in %q", got, want)
	}
}
