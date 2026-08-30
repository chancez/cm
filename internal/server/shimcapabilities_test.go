package server

import (
	"context"
	"log/slog"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/chancez/cm/internal/capability"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// capabilityShim answers State with a fixed capability list and records what Shutdown was sent.
//
// A fake rather than a real shim, because the case under test is a *build* difference: a shim that
// predates a field cannot be produced by configuring the current one, and the whole point is what the
// server does when it meets one.
type capabilityShim struct {
	shimv1.ShimClient
	capabilities []string
	states       int
	shutdowns    []*shimv1.ShutdownRequest
}

func (c *capabilityShim) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	c.states++
	return &shimv1.StateResponse{Capabilities: c.capabilities}, nil
}

func (c *capabilityShim) Shutdown(_ context.Context, req *shimv1.ShutdownRequest) (*shimv1.ShutdownResponse, error) {
	c.shutdowns = append(c.shutdowns, req)
	return &shimv1.ShutdownResponse{}, nil
}

// TestAShimThatMayNotHonorAShutdownSignalIsReported covers the substitution shim.proto claimed was logged
// and was not.
//
// ShutdownRequest.signal is additive, so a shim predating it ignores the field and derives the signal from
// force alone: SIGHUP, or SIGKILL when forced. `cm kill --signal TERM` against such a shim therefore sent
// SIGHUP and reported success. That matters because the reason to name a signal is that the job traps it,
// so something catching SIGTERM to finish writing a file got hung up on instead, and no log anywhere said
// the signal had been swapped.
//
// Three outcomes, all reachable, which is why the answer is capability.Support and not a bool. A shim that
// reports the capability is silent; one that reports capabilities without it is a conclusive no; one that
// reports nothing predates the mechanism, which is every shim running today, and gets a hedge rather than
// a claim.
func TestAShimThatMayNotHonorAShutdownSignalIsReported(t *testing.T) {
	tests := []struct {
		name         string
		capabilities []string
		// want is a substring the log must contain, and unwanted one it must not.
		want     string
		unwanted string
	}{
		{
			name:         "a shim reporting the capability is trusted silently",
			capabilities: capability.Shim().Strings(),
			unwanted:     "shutdown signal",
		},
		{
			name:         "a shim reporting capabilities without this one is a conclusive no",
			capabilities: []string{string(capability.Reported)},
			want:         "does not honor an explicit shutdown signal",
		},
		{
			name:         "a shim reporting nothing predates the mechanism and cannot be concluded about",
			capabilities: nil,
			want:         "cannot confirm",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shim := &capabilityShim{capabilities: tt.capabilities}
			var logged lockedBuffer
			sess := &Session{
				id:   "s1",
				shim: shim,
				log:  slog.New(slog.NewTextHandler(&logged, nil)),
			}

			if _, err := sess.Shutdown(context.Background(), false, int32(syscall.SIGTERM)); err != nil {
				t.Fatalf("Shutdown() error = %v", err)
			}

			got := logged.String()
			if tt.want != "" && !strings.Contains(got, tt.want) {
				t.Errorf("the log does not mention %q.\nlog: %s", tt.want, got)
			}
			if tt.unwanted != "" && strings.Contains(got, tt.unwanted) {
				t.Errorf("the log mentions %q about a shim that reported the capability, which is noise on "+
					"the healthy path.\nlog: %s", tt.unwanted, got)
			}
			// The warning is about what the shim will do with the request, so the request itself must be
			// unchanged in every case: a capability check that altered what went on the wire would be a
			// behavior change hiding inside a diagnostic.
			want := []*shimv1.ShutdownRequest{{Force: false, Signal: int32(syscall.SIGTERM)}}
			if !reflect.DeepEqual(shim.shutdowns, want) {
				t.Errorf("shim received %v, want %v", shim.shutdowns, want)
			}
		})
	}
}

// The signal actually substituted is named, so the log says what the shell got rather than only that
// something was wrong. Forced and unforced pick different ones, which is the part a reader cannot guess.
func TestTheSubstitutedShutdownSignalIsNamed(t *testing.T) {
	tests := []struct {
		name  string
		force bool
		want  string
	}{
		{"unforced falls back to SIGHUP", false, syscall.SIGHUP.String()},
		{"forced falls back to SIGKILL", true, syscall.SIGKILL.String()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shim := &capabilityShim{capabilities: []string{string(capability.Reported)}}
			var logged lockedBuffer
			sess := &Session{
				id:   "s1",
				shim: shim,
				log:  slog.New(slog.NewTextHandler(&logged, nil)),
			}

			if _, err := sess.Shutdown(context.Background(), tt.force, int32(syscall.SIGTERM)); err != nil {
				t.Fatalf("Shutdown() error = %v", err)
			}

			got := logged.String()
			if !strings.Contains(got, tt.want) {
				t.Errorf("the log does not name %q as what was sent instead.\nlog: %s", tt.want, got)
			}
			if !strings.Contains(got, syscall.SIGTERM.String()) {
				t.Errorf("the log does not name %q as what was asked for.\nlog: %s",
					syscall.SIGTERM.String(), got)
			}
		})
	}
}

// A shutdown that names no signal asks the shim nothing extra.
//
// Zero means "not specified", so force alone decides and there is nothing to substitute. Checked because
// the capability lookup costs a round trip on a shim that has not been asked yet, and paying it on the
// ordinary kill path would be a cost with no question behind it.
func TestAShutdownWithoutASignalDoesNotProbeTheShim(t *testing.T) {
	shim := &capabilityShim{capabilities: capability.Shim().Strings()}
	var logged lockedBuffer
	sess := &Session{
		id:   "s1",
		shim: shim,
		log:  slog.New(slog.NewTextHandler(&logged, nil)),
	}

	if _, err := sess.Shutdown(context.Background(), true, 0); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}

	if shim.states != 0 {
		t.Errorf("the shim was asked for its state %d times during a shutdown that named no signal, want 0",
			shim.states)
	}
	if got := logged.String(); got != "" {
		t.Errorf("a shutdown naming no signal logged %q, want nothing: there is no substitution to report",
			got)
	}
}

// TestAShimsCapabilitiesAreAskedForOnce covers the caching, which is what keeps the gate cheap.
//
// A shim runs one binary for its whole life, so its capabilities are fixed from the moment it starts.
// Asking again per call would put a round trip on paths that have one already.
func TestAShimsCapabilitiesAreAskedForOnce(t *testing.T) {
	shim := &capabilityShim{capabilities: capability.Shim().Strings()}
	sess := &Session{id: "s1", shim: shim, log: slog.New(slog.NewTextHandler(&lockedBuffer{}, nil))}
	ctx := context.Background()

	first := sess.ShimCapabilities(ctx)
	second := sess.ShimCapabilities(ctx)

	if shim.states != 1 {
		t.Errorf("the shim was asked for its state %d times across two lookups, want 1", shim.states)
	}
	if !reflect.DeepEqual(first.Names(), second.Names()) {
		t.Errorf("the cached answer %v differs from the first %v", second.Names(), first.Names())
	}
	if got, want := first.Names(), capability.Shim().Names(); !reflect.DeepEqual(got, want) {
		t.Errorf("ShimCapabilities() = %v, want %v", got, want)
	}
}

// A State call made for another reason populates the cache, so the gate usually costs nothing at all.
func TestAStateCallRecordsTheShimsCapabilities(t *testing.T) {
	shim := &capabilityShim{capabilities: capability.Shim().Strings()}
	sess := &Session{id: "s1", shim: shim, log: slog.New(slog.NewTextHandler(&lockedBuffer{}, nil))}
	ctx := context.Background()

	if _, err := sess.State(ctx); err != nil {
		t.Fatalf("State() error = %v", err)
	}
	got := sess.ShimCapabilities(ctx)

	if shim.states != 1 {
		t.Errorf("the shim was asked for its state %d times, want 1: the State call above already "+
			"carried the capabilities", shim.states)
	}
	if want := capability.Shim().Names(); !reflect.DeepEqual(got.Names(), want) {
		t.Errorf("ShimCapabilities() = %v, want %v", got.Names(), want)
	}
}
