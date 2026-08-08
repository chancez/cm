//go:build !cm_test_version

package paths

// versionOverride reports no override, because this is a normal build.
//
// The override exists only in a binary built with the cm_test_version tag. Gating it on a build tag rather
// than on an environment variable alone is the point: an env var a released binary honors is one a stale
// `export CM_VERSION` in a shell profile can make every version report a lie, and one that someone could set
// to match on purpose and silence the skew warning that exists to catch a real mismatch. Neither is possible
// if the code that reads it is not in the binary.
//
// Inlined to nothing, so this costs a released build no branch.
func versionOverride() string { return "" }
