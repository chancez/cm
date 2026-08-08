package paths

import (
	"runtime/debug"
	"strings"
)

// Version reports what build this binary is, for diagnostics and for spotting a mismatch.
//
// Read from the Go build info rather than stamped with ldflags, so a `go build` with no special flags still
// produces something meaningful and there is no build incantation to remember.
//
// A real tag wins; a pseudo-version does not. Main.Version is v0.0.1 when built from a tag, which is what a
// person wants to read, but v0.0.0-20260808150353-d83057a441e1 otherwise -- the same commit hash padded with
// noise, where the bare hash is clearer. So a version starting with "v" and containing no "-" is used as-is,
// and anything else falls through to the commit.
//
// Why cm needs this at all: a session outlives the server that created it, and a client can be a different
// build from the server it talks to. Protobuf ignores unknown fields, so a newer client asking an older
// server for something it does not implement gets a zero value rather than an error, which looks like a bug
// in the feature rather than a version difference. Being able to compare the two turns that into a warning.
//
// Can be overridden, but only in a binary built with the cm_test_version tag. See versionOverride.
func Version() string {
	// Checked first, so an override wins over anything the build recorded. Compiled out entirely without the
	// build tag, so a released binary has no way to be told it is a version it is not.
	if v := versionOverride(); v != "" {
		return v
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	// A tagged build reports its tag, which is more useful than a hash. A pseudo-version is not a tag: it is
	// generated for an untagged commit and carries the same hash this function would print anyway.
	if v := info.Main.Version; isReleaseVersion(v) {
		return v
	}

	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		// No VCS stamps, which happens when building from a module cache or with -buildvcs=false. Anything
		// Main.Version says beats nothing here, including a pseudo-version, since it at least carries the
		// commit.
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
		return "devel"
	}
	// Short revision, since the full hash is noise in a log line or a table.
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		// Marked, because a dirty build is the one where "which code is this?" matters most.
		//
		// Not reliable in a git worktree: Go reports vcs.modified=false there even with tracked files
		// modified, while the same change in the main checkout reports true. Verified on go 1.26. So a
		// missing marker means "clean, or built from a worktree" rather than simply clean, and the version
		// check treats any difference as skew rather than relying on this.
		return revision + "-dirty"
	}
	return revision
}

// isReleaseVersion reports whether a module version is a real tag rather than a generated pseudo-version.
//
// A pseudo-version always carries a timestamp and revision after a dash, as in
// v0.0.0-20260808150353-d83057a441e1, so the absence of a dash distinguishes v0.0.1 from it.
//
// Two consequences worth naming. Go appends "+dirty" to a tagged version built from a modified tree, which has
// no dash and so is accepted -- correctly, since "v0.0.1+dirty" is exactly what someone needs to see. And a
// pre-release tag like v1.0.0-rc1 does contain a dash and falls through to the commit hash, which loses a
// little precision in exchange for never mistaking generated noise for a release.
func isReleaseVersion(v string) bool {
	return strings.HasPrefix(v, "v") && !strings.Contains(v, "-")
}

// VersionEnvSuffix names the environment variable that overrides the reported version in a test build.
//
// Exported so tests spell it through Env rather than hardcoding "CM_VERSION", which would break silently if
// the prefix changed. Declared here rather than in the tagged file so a test can reference the name whether or
// not the tag is set, and so this comment is visible when reading Version.
const VersionEnvSuffix = "VERSION"

// VersionBuildTag is the build tag that enables the override.
//
// Exported so a test harness building an instrumented binary names it from here rather than repeating the
// string, which is the sort of duplication that turns "the override did not take effect" into a passing test.
const VersionBuildTag = "cm_test_version"
