package paths

import "testing"

// A real tag is preferred over a commit hash; a generated pseudo-version is not.
//
// The distinction matters because Main.Version is populated either way. From a tag it reads v0.0.1, which is
// what a person wants; otherwise it is v0.0.0-20260808150353-d83057a441e1, the same commit hash this function
// would print anyway, padded with a timestamp.
func TestIsReleaseVersion(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    bool
	}{
		{version: "v0.0.1", want: true},
		{version: "v1.2.3", want: true},
		// Go appends this to a tagged version built from a modified tree. Accepted deliberately: it is the
		// most useful thing to show, since it names the tag and says the build does not match it.
		{version: "v0.0.1+dirty", want: true},
		// A pseudo-version: a timestamp and revision after a dash. Rejected, since the hash it carries is
		// what the caller falls back to anyway, without the noise.
		{version: "v0.0.0-20260808150353-d83057a441e1", want: false},
		// A pre-release tag also contains a dash, so it falls through to the commit. Less precise, and worth
		// it to never mistake a pseudo-version for a release.
		{version: "v1.0.0-rc1", want: false},
		{version: "(devel)", want: false},
		{version: "", want: false},
		// No leading v, so not a module version at all.
		{version: "0.0.1", want: false},
	} {
		if got := isReleaseVersion(tc.version); got != tc.want {
			t.Errorf("isReleaseVersion(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// Version always returns something usable, since it is printed in diagnostics.
//
// An empty string would make `cm doctor` report "client , server " and would make the version-skew check
// compare nothing against nothing.
func TestVersionIsNeverEmpty(t *testing.T) {
	if got := Version(); got == "" {
		t.Error("Version() = \"\", want something identifying the build")
	}
}
