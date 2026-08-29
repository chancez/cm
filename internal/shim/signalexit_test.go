package shim

import (
	"strconv"
	"testing"
)

// A shell killed by a signal reports 128+signal, the way a shell does.
//
// Go's ExitCode returns -1 for a process "terminated by a signal", which is the same value it returns for
// one that has not exited, and the server reads a negative code as "no status is available, the session is
// lost". So a signal death was recorded as a lost session with exit code 0: `cm ls` reported success for a
// session that had been killed, and `cm run` collapsed TERM, KILL and INT to exit 1 with "ended
// unexpectedly".
//
// Measured against /bin/sh, which is the only correct reference here since `cm run` documents itself as
// usable in a script the way a local command is: sh gives 143, 137 and 130 for those three signals.
func TestSignalDeathReportsTheShellConvention(t *testing.T) {
	for _, tc := range []struct {
		name string
		sig  string
		want int
	}{
		{name: "SIGTERM", sig: "TERM", want: 143},
		{name: "SIGKILL", sig: "KILL", want: 137},
		{name: "SIGINT", sig: "INT", want: 130},
		{name: "SIGHUP", sig: "HUP", want: 129},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := Start(Config{
				Session: "sig",
				// The shell signals itself, so the death is the command's own rather than something the
				// test does to it from outside. That is the case a script cares about.
				Command: []string{"/bin/sh", "-c", "kill -" + tc.sig + " $$"},
				Rows:    24, Cols: 80,
			})
			if err != nil {
				t.Fatalf("Start() error = %v", err)
			}

			if got := waitExit(t, s); got != tc.want {
				t.Errorf("exit code for a shell killed by %s = %d, want %d: /bin/sh reports %d, and a "+
					"negative or zero code here is read by the server as a session it lost rather than "+
					"one that was killed", tc.sig, got, tc.want, tc.want)
			}
		})
	}
}

// A plain nonzero exit is unchanged, which is the control: the fix must not move the codes that already
// worked, and those are the overwhelmingly common case.
func TestPlainExitCodesAreUnchanged(t *testing.T) {
	for _, want := range []int{0, 1, 7, 42, 255} {
		s, err := Start(Config{
			Session: "plain",
			Command: []string{"/bin/sh", "-c", "exit " + strconv.Itoa(want)},
			Rows:    24, Cols: 80,
		})
		if err != nil {
			t.Fatalf("Start() error = %v", err)
		}
		if got := waitExit(t, s); got != want {
			t.Errorf("exit code for `exit %d` = %d, want %d", want, got, want)
		}
	}
}
