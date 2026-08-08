//go:build !darwin && !linux

package paths

// BootID reports no boot identifier on a platform that has not been taught one.
//
// A stub rather than a build failure, so cm compiles somewhere new. The consequence is one field missing from a
// log line, which is the right trade against inventing a value that means nothing.
func BootID() string { return "" }
