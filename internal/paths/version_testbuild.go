//go:build cm_test_version

package paths

import "os"

// versionOverride reports a version set in the environment, for a test build only.
//
// This file is compiled only under the cm_test_version tag, so a released binary cannot be told it is a
// version it is not. See versionOverride in version_fixed.go for why that matters.
//
// It exists because the behavior worth testing is what happens when a client and a server disagree, and
// without an override the only way to produce a disagreement is to build two binaries from two different tags.
// That is a manual procedure, not a test: it cannot run in CI, it silently tests nothing if the tags are
// missing, and it needs a git operation to set up.
//
// Read on every call rather than cached at init, since a test that sets it after the process starts would
// otherwise see the real version and pass for the wrong reason.
func versionOverride() string { return os.Getenv(Env(VersionEnvSuffix)) }
