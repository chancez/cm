package server

import (
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chancez/cm/internal/cmlog"
)

// The compactor offers the model a step every compactInterval, runs a pass to the end once one starts, stops
// for good when a step fails, and ends with the session.
func TestRunCompactor(t *testing.T) {
	type calls struct {
		afterFirstTick int
		afterPass      int
		afterFailure   int
		afterFailTick  int
	}
	synctest.Test(t, func(t *testing.T) {
		term := &fakeTerminal{}
		sess := &Session{id: "w", done: make(chan struct{}), term: term, log: cmlog.Discard()}
		go sess.runCompactor()
		count := func() int {
			synctest.Wait()
			term.mu.Lock()
			defer term.mu.Unlock()
			return term.compactCalls
		}
		var got calls

		time.Sleep(compactInterval)
		got.afterFirstTick = count()

		// A pass of three steps with more work, then the one that reports none, all within one tick.
		term.mu.Lock()
		term.compactWork = 3
		term.mu.Unlock()
		time.Sleep(compactInterval)
		got.afterPass = count()

		term.mu.Lock()
		term.compactErr = errors.New("boom")
		term.mu.Unlock()
		time.Sleep(compactInterval)
		got.afterFailure = count()
		time.Sleep(10 * compactInterval)
		got.afterFailTick = count()

		want := calls{afterFirstTick: 1, afterPass: 5, afterFailure: 6, afterFailTick: 6}
		if got != want {
			t.Errorf("Compact calls = %+v, want %+v", got, want)
		}
	})
}

// The compactor ends with the session even while compression has work left, rather than holding the bubble.
func TestRunCompactorEndsWithSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		term := &fakeTerminal{}
		sess := &Session{id: "w", done: make(chan struct{}), term: term, log: cmlog.Discard()}
		go sess.runCompactor()
		time.Sleep(compactInterval)
		close(sess.done)
		// synctest.Test fails if runCompactor is still running when the bubble's function returns.
		synctest.Wait()
	})
}
