package main

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/capability"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// TestAWaitIsRefusedOnlyWhenTheServerSaidItCannot is the asymmetry the whole gate rests on.
//
// Absent is a conclusive no and is worth refusing. Unknown is not: it is what every server predating
// capability reporting sends, which today is all of them, so refusing on silence would turn `cm wait
// --until blocked` from working into failing on the strength of not having asked in time.
func TestAWaitIsRefusedOnlyWhenTheServerSaidItCannot(t *testing.T) {
	tests := []struct {
		name    string
		caps    capability.Set
		wantErr bool
	}{
		{
			name:    "a server that reports the capability is allowed",
			caps:    capability.Server(),
			wantErr: false,
		},
		{
			name:    "a server that reports capabilities without this one is refused",
			caps:    capability.Parse([]string{string(capability.Reported)}),
			wantErr: true,
		},
		{
			name:    "a server that reports nothing is allowed, since silence is not refusal",
			caps:    capability.Parse(nil),
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := requireServerCapability(tt.caps, capability.WaitReportedState, "wait for the blocked state")

			if (err != nil) != tt.wantErr {
				t.Fatalf("requireServerCapability() error = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil {
				return
			}
			// The remedy has to be in the message. "Unsupported" alone leaves the reader guessing whether the
			// fix is a flag, an upgrade or a restart, and it is a restart: the new binary is already on disk.
			for _, want := range []string{"server restart", string(capability.WaitReportedState)} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestOnlyTheWaitFormsThatDependOnAServerAskForOne pins which forms pay for a probe.
//
// Idle, busy and exited come from OSC 133 and the session's own state, which every server has always
// done, so a probe for them would be a round trip buying nothing. Getting this table wrong in the
// permissive direction is invisible, which is why it is asserted rather than left to the code.
func TestOnlyTheWaitFormsThatDependOnAServerAskForOne(t *testing.T) {
	tests := []struct {
		name   string
		target waitTarget
		want   capability.Name
	}{
		{"blocked needs reported state", waitTarget{state: serverv1.WaitState_WAIT_STATE_BLOCKED}, capability.WaitReportedState},
		{"a match needs match support", waitTarget{match: "DONE"}, capability.WaitMatch},
		{"idle needs nothing", waitTarget{state: serverv1.WaitState_WAIT_STATE_IDLE}, ""},
		{"busy needs nothing", waitTarget{state: serverv1.WaitState_WAIT_STATE_BUSY}, ""},
		{"exited needs nothing", waitTarget{state: serverv1.WaitState_WAIT_STATE_EXITED}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, what := tt.target.needsCapability()

			if got != tt.want {
				t.Errorf("needsCapability() = %q, want %q", got, tt.want)
			}
			// A dependency with no phrase would produce a refusal reading "cannot : it does not implement".
			if (what == "") != (tt.want == "") {
				t.Errorf("needsCapability() phrase = %q alongside capability %q; a dependency needs both or "+
					"neither", what, got)
			}
		})
	}
}

// TestATimeoutExplainsAnUnconfirmedCapability covers what the Unknown case buys.
//
// The gate cannot refuse an unreporting server, so the doubt has to surface somewhere, and the only
// useful moment is the failure: a wait that could never have been satisfied looks exactly like one that
// timed out honestly. checks.go records that ambiguity costing a bad hour.
func TestATimeoutExplainsAnUnconfirmedCapability(t *testing.T) {
	tests := []struct {
		name string
		caps capability.Set
		want string
	}{
		{
			name: "a server that reports the capability adds nothing",
			caps: capability.Server(),
			want: "",
		},
		{
			name: "a server that reports nothing says it may predate this",
			caps: capability.Parse(nil),
			want: "may predate",
		},
		{
			name: "a server that reports capabilities without this one says so outright",
			caps: capability.Parse([]string{string(capability.Reported)}),
			want: "does not implement",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := explainUnsatisfied(tt.caps, capability.WaitReportedState)

			switch {
			case tt.want == "" && got != "":
				t.Errorf("explainUnsatisfied() = %q, want nothing: a genuine timeout should not be muddied "+
					"by a compatibility note that does not apply", got)
			case tt.want != "" && !strings.Contains(got, tt.want):
				t.Errorf("explainUnsatisfied() = %q, want it to mention %q", got, tt.want)
			}
		})
	}
}
