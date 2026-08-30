package main

import (
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
)

// A handover is only honored by the process it was made for.
//
// The pid is what keeps the environment from becoming the bug the argv was. A position that reaches a
// process it does not describe makes the server skip both the screen repaint and the sizing, so the
// window paints nothing and keeps the size it had, which is what a replayed --resume-from-seq did. exec
// preserves the pid, so a handover this process made to itself matches and nothing else does.
func TestParseResumeFrom(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		pid  int
		want *uint64
	}{
		{
			name: "a handover to this process is honored",
			spec: "4242:106",
			pid:  4242,
			want: new(uint64(106)),
		},
		{
			// The stale-export case, and the reason for the pid at all: a value left in a shell profile
			// or copied into an environment belongs to no live re-exec.
			name: "a handover to another process is ignored",
			spec: "4242:106",
			pid:  9182,
			want: nil,
		},
		{
			// Zero asks for the whole retained log, which is the opposite of resuming.
			name: "a zero position is not a resume point",
			spec: "4242:0",
			pid:  4242,
			want: nil,
		},
		{
			name: "a bare position with no pid is ignored",
			spec: "106",
			pid:  4242,
			want: nil,
		},
		{
			name: "a position that is not a number is ignored",
			spec: "4242:later",
			pid:  4242,
			want: nil,
		},
		{
			name: "an empty value is ignored",
			spec: "",
			pid:  4242,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := parseResumeFrom(tc.spec, tc.pid)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parseResumeFrom(%q, %d) = %v, want %v",
					tc.spec, tc.pid, derefResume(got), derefResume(tc.want))
			}
		})
	}
}

// The variable is cleared whether or not its value was usable.
//
// Clearing is not tidiness. `cm attach` forwards this process's whole environment to a session it
// creates, so a variable still set here is exported in that session's shell and inherited by every cm
// command run inside it, and each of those would attach believing it had been handed a position.
func TestTakeResumeFromClearsTheVariable(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec string
		pid  int
		want *uint64
	}{
		{
			name: "a usable handover",
			spec: "4242:106",
			pid:  4242,
			want: new(uint64(106)),
		},
		{
			name: "a handover for another process",
			spec: "9182:106",
			pid:  4242,
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(resumeEnvVar, tc.spec)

			got := takeResumeFrom(tc.pid)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("takeResumeFrom(%d) = %v, want %v",
					tc.pid, derefResume(got), derefResume(tc.want))
			}
			if v, ok := os.LookupEnv(resumeEnvVar); ok {
				t.Errorf("%s is still set to %q after being taken", resumeEnvVar, v)
			}
		})
	}
}

// Nothing is reported when no handover was made, which is every ordinary attach.
func TestTakeResumeFromWithNoHandover(t *testing.T) {
	// Set and cleared rather than assumed absent, so the test does not depend on the environment it
	// runs in. t.Setenv also fails a parallel test, which is the guard against another test racing this
	// one on the same variable.
	t.Setenv(resumeEnvVar, "")
	if err := os.Unsetenv(resumeEnvVar); err != nil {
		t.Fatalf("unsetting %s: %v", resumeEnvVar, err)
	}

	if got := takeResumeFrom(os.Getpid()); got != nil {
		t.Errorf("takeResumeFrom() = %d with no handover made, want nil", *got)
	}
}

// The environment handed to the replacement carries exactly one handover.
func TestResumeEnviron(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "appended to the existing environment",
			env:  []string{"TERM=xterm-kitty", "SHELL=/bin/zsh"},
			want: []string{"TERM=xterm-kitty", "SHELL=/bin/zsh", "CM_RESUME_FROM_SEQ=4242:106"},
		},
		{
			// Two entries for one name leave which of them wins up to whatever reads it.
			name: "an existing handover is replaced rather than repeated",
			env:  []string{"TERM=xterm-kitty", "CM_RESUME_FROM_SEQ=4242:11"},
			want: []string{"TERM=xterm-kitty", "CM_RESUME_FROM_SEQ=4242:106"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := resumeEnviron(tc.env, 4242, 106)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("resumeEnviron() = %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The position survives a round trip through the environment, which is the whole handover.
func TestResumeHandoverRoundTrip(t *testing.T) {
	const pid = 4242

	env := resumeEnviron([]string{"TERM=xterm-kitty"}, pid, 106)

	// Read back the way the replacement reads it, from the entry rather than from the real environment,
	// so this asserts on the format both sides agree on rather than on os.Environ.
	var spec string
	for _, kv := range env {
		if after, ok := strings.CutPrefix(kv, resumeEnvVar+"="); ok {
			spec = after
		}
	}
	got := parseResumeFrom(spec, pid)
	if want := new(uint64(106)); !reflect.DeepEqual(got, want) {
		t.Errorf("round trip gave %v, want 106", derefResume(got))
	}
}

// The name the client sets and the name a session refuses to inherit must be the same string.
//
// Two literals in two packages, and drift between them is silent: the client would keep handing over a
// variable that sessions then inherit, so every cm command inside a session created by an upgraded
// client would read a position meant for one process. sessionenv cannot import cmd/cm, so this is what
// holds them together.
func TestResumeVariableIsNotInherited(t *testing.T) {
	if !slices.Contains(sessionenv.NoInherit, resumeEnvVar) {
		t.Errorf("sessionenv.NoInherit = %q, which does not list %s",
			sessionenv.NoInherit, resumeEnvVar)
	}
}

// The handover must not be bound to the flag it is named after.
//
// bindEnv derives a CM_-prefixed variable from every flag name, and this variable is named after
// --resume-from-seq on purpose, so the convention derives exactly it. Bound, the handover reaches the
// flag as its value: "<pid>:<seq>" is not a uint64, so every re-exec'd client exited with
// `invalid argument "78714:37"` before attaching and left the window holding a dead terminal. Caught by
// an existing e2e test rather than by review, which is why it is pinned here at the level where the
// collision is visible.
func TestResumeVariableIsNotBoundToTheFlag(t *testing.T) {
	// Derived the way bindEnv derives it, so this notices if either name moves.
	key := paths.Env(strings.ToUpper(strings.ReplaceAll(resumeFromFlag, "-", "_")))
	if key == resumeEnvVar && !noEnvFlags[resumeFromFlag] {
		t.Errorf("%s is the variable bindEnv derives from --%s, and the flag is not in noEnvFlags, so the "+
			"handover is parsed as the flag's value and every upgraded client dies on it",
			resumeEnvVar, resumeFromFlag)
	}
}

// derefResume renders a position for a failure message, since a pointer prints as an address.
func derefResume(p *uint64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatUint(*p, 10)
}
