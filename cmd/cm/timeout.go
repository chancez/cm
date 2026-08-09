package main

import (
	"context"
	"time"

	"github.com/spf13/pflag"
)

// timeoutUsage is the help text every --timeout shares.
//
// One string rather than a phrase per command, because the flag has to mean the same thing everywhere.
// It previously existed on three commands and was absent from the one that could block forever, which is
// the kind of drift a shared definition prevents.
const timeoutUsage = "give up after this long (0 waits indefinitely)"

// addTimeoutFlag registers --timeout on a command's flag set.
func addTimeoutFlag(f *pflag.FlagSet, d *time.Duration) {
	f.DurationVar(d, "timeout", 0, timeoutUsage)
}

// withTimeout derives a context bounded by d, or returns ctx unchanged when d is zero.
//
// A helper rather than an inline WithTimeout at each site because zero has to keep meaning "no bound":
// context.WithTimeout(ctx, 0) produces an already-expired deadline, so passing an unset flag straight
// through would make every command fail instantly. That is an easy mistake to make once per call site and
// impossible to make here.
//
// The cancel function is always returned and always safe to call, so callers can defer it without
// checking whether a deadline was set.
func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}
