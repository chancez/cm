//go:build cgo

package vt

import (
	"sync"

	"go.mitchellh.com/libghostty"
)

// LibraryVersion reports the version of the libghostty-vt linked into this binary, such as
// "1.1.0-dev+5813c9461d5a", or "unknown" if the library will not say.
//
// Asked of the library rather than recorded at build time, so it cannot disagree with what is actually
// linked. The commit after the "+" is there because scripts/libghostty.sh puts it there: ghostty takes it
// from git, and the script builds from an export with no repository, so a library built any other way
// may report the bare version. libghostty is unstable and pinned to a commit rather than a release, so
// "1.1.0-dev" alone does not say which build a report came from.
var LibraryVersion = sync.OnceValue(func() string {
	info, err := libghostty.GetBuildInfo()
	if err != nil || info.VersionString == "" {
		return "unknown"
	}
	return info.VersionString
})
