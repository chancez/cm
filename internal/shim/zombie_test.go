package shim

import (
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A live process is reported as living, and a reaped one is not.
//
// The two ends of the range, so the interesting case below is not the only coverage.
func TestProcessLivesOnRunningAndGoneProcesses(t *testing.T) {
	if !processLives(os.Getpid()) {
		t.Error("processLives(self) = false, want true for a process that is plainly running")
	}

	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a short-lived process: %v", err)
	}
	pid := cmd.Process.Pid
	// Waited, so the entry is collected rather than left as a zombie.
	if err := cmd.Wait(); err != nil {
		t.Fatalf("waiting for it: %v", err)
	}
	if processLives(pid) {
		t.Errorf("processLives(%d) = true after the process exited and was reaped, want false", pid)
	}
}

// A zombie is not living, even though its pid still exists.
//
// This is the bug the function exists for. A process that has been SIGKILLed but not yet reaped keeps its
// pid, so signal 0 succeeds and the leak check called it a survivor of SIGKILL, which nothing can survive.
// It presented as `cm kill --signal kill` warning about a process it had just killed, and as `cm doctor`
// never going quiet after a clean kill.
//
// Runs on both platforms, which matters: this was found on Linux and looked platform-specific, but a zombie
// is observable on darwin too and a /proc-only fix would have left it broken there while appearing correct.
// The check reads /proc where it exists and asks ps otherwise, so this exercises whichever applies.
func TestProcessLivesTreatsAZombieAsGone(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting a process to leave unreaped: %v", err)
	}
	pid := cmd.Process.Pid

	// Deliberately not calling Wait, which is what makes this a zombie rather than a gone process.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if syscall.Kill(pid, 0) != nil {
			t.Skip("the process was reaped before it could be observed as a zombie")
		}
		if !processLives(pid) {
			// A pid that exists but does not live: exactly the state being tested.
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("processLives(%d) still true after 5s, want false once it became a zombie; "+
				"a SIGKILLed process would be reported as having survived SIGKILL", pid)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Cleaned up so the test does not leak the entry into whatever runs next.
	_ = cmd.Wait()
}

// SIGKILL to a live job reports no survivors.
//
// The end-to-end shape of the same bug at the unit level, without a server or a pty: what the leak check
// reports is what the warning prints, so a false survivor here is a false warning to the user.
// Through SignalAndCheck rather than by reimplementing its loop. An earlier version of this test called
// processLives directly, which meant it passed with the fix mutated back out: it was testing the helper it
// had been told to use rather than what the shim actually does.
//
// It only stands guard on Linux. Verified by mutation: with the zombie-blind check restored this fails there
// with "surviving = [4867] after SIGKILL to group 4866", and still passes on darwin, where the pty teardown
// reaps the job before the check runs. Kept running on both anyway, since it costs a second and the fallback
// path it covers is the darwin one, but `mise run test-linux` is what actually holds this down.
func TestSignalAndCheckReportsNoSurvivorsAfterSIGKILL(t *testing.T) {
	sess, err := Start(Config{
		Session: "zombie",
		// A shell that spawns a child, so the group has more than one member and the check has something
		// to get wrong.
		Command: []string{"/bin/sh", "-c", "sleep 300"},
		Rows:    24, Cols: 80,
	})
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() { _, _, _ = sess.SignalAndCheck(syscall.SIGKILL, 0) })

	// Let the shell get as far as spawning sleep.
	time.Sleep(400 * time.Millisecond)

	pgid, surviving, err := sess.SignalAndCheck(syscall.SIGKILL, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("SignalAndCheck() error = %v", err)
	}
	if pgid <= 0 {
		t.Fatalf("pgid = %d, want the group that was signalled", pgid)
	}
	// Nothing survives SIGKILL, so a non-empty list here is the bug: unreaped entries counted as running
	// processes, which reached the user as `cm kill --signal kill` warning about a process it had killed.
	if len(surviving) != 0 {
		t.Errorf("surviving = %v after SIGKILL to group %d, want none", surviving, pgid)
	}
}
