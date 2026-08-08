package paths

import "runtime/debug"

// Version reports what build this binary is, for diagnostics and for spotting a mismatch.
//
// Read from the Go build info rather than stamped with ldflags, so a `go build` with no special flags still
// produces something meaningful and there is no build incantation to remember.
//
// The VCS revision is preferred over Main.Version even when both exist, because Main.Version for anything not
// installed from a tag is a pseudo-version like v0.0.0-20260808150353-d83057a441e1: it contains the same
// commit hash padded with noise, and the bare hash is what someone comparing two builds actually reads.
//
// Why cm needs this at all: a session outlives the server that created it, and a client can be a different
// build from the server it talks to. Protobuf ignores unknown fields, so a newer client asking an older
// server for something it does not implement gets a zero value rather than an error, which looks like a bug
// in the feature rather than a version difference. Being able to compare the two turns that into a warning.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
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
		// No VCS stamps, which happens when building from a module cache or with -buildvcs=false. A tagged
		// install has a real version to fall back to; a plain `go build` reports "(devel)", which says
		// nothing.
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
