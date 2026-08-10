---
name: cm-verify-fix
description: "Prove a fix in cm actually fixes the bug, and that its test would have caught it. Use after writing any bug fix, and when a test fails intermittently. Covers reverting to check the test fails, mutation testing safely, and turning a race into something deterministic."
---

# Verifying a fix

A fix that is not verified against the failing behavior is a guess, and cm's bugs are mostly the kind
that pass a casual check: they need a short terminal, a differing client size, a non-tty stdin, or a
window a few hundred milliseconds wide. Several "fixes" here were wrong in ways only a control
measurement revealed.

This is the routine that has actually worked.

## 1. Reproduce before fixing, and record the rate

Get a number. "Fails sometimes" is not a baseline, so there is nothing to compare against afterwards.

```sh
fails=0
for i in $(seq 1 25); do
  go test ./internal/e2e/ -run 'TestName' -count=1 >/dev/null 2>&1 || fails=$((fails+1))
done
echo "$fails / 25"
```

For an intermittent failure, first establish whether it is deterministic **without** `-race`, and say
which. The race detector widens real windows, so a failure only under `-race` is still a real bug, and
knowing that shapes where to look.

## 2. Find the mechanism, do not just move the symptom

Read the code path the error message names. That has located cm bugs faster than trying to reproduce
harder, every time. `stopping %s: %w` in one case pointed straight at the early return that was skipping
a store delete.

**Confirm the mechanism before writing the fix.** The cheapest confirmation is usually to make the bug
deterministic by widening the window you think is responsible: insert a `time.Sleep` at the suspected
gap and see whether the failure becomes reliable. If it does not, the theory is wrong.

That check saved a wrong fix here: a doubled character in a session's output looked like input arriving
before zsh's line editor was ready, and a fix built on that premise made the failure rate *worse*
(1-in-10 to 6-in-25). The actual cause was a stream being cancelled before its output was read.

Back the theory out afterwards. Use `cp file /tmp/x.bak` and restore from the copy -- never
`git checkout`, which deletes the uncommitted work in that file along with the probe.

## 3. Verify the test fails without the fix

This is the step that gets skipped, and it is the one that matters.

```sh
cp internal/server/thing.go /tmp/thing.bak
# revert just the fix, by hand or with a targeted edit
go test ./internal/... -run 'TestTheRegression' -count=1   # must FAIL
cp /tmp/thing.bak internal/server/thing.go
go test ./internal/... -run 'TestTheRegression' -count=1   # must PASS
```

Then check `git diff --stat` and confirm only what you intended has changed.

If the test still passes with the fix reverted, the test is not testing the fix. That happened here:
a regression test passed against broken code because a generous `sleep` let a fallback path resolve
things.

## 4. If it is a race, measure the test's power

A regression test for a race must catch it reliably, or it is decoration. Measure:

```sh
# with the fix reverted, how often does the new test actually fail?
fails=0
for i in $(seq 1 10); do go test ./... -run 'TestTheRegression' -count=1 >/dev/null 2>&1 || fails=$((fails+1)); done
echo "$fails / 10"
```

One in six is not standing guard. When that happens, move the assertion down to where the state can be
*constructed* instead of raced for:

- Build the exact intermediate state by hand. `internal/server/readended_test.go` marks a session ended
  while leaving it registered, which is the window the bug lived in, deterministically.
- Or wait for the state rather than racing it: `<-sess.Done()` then act.

Keep the e2e test for coverage, and say in its comment that it is probabilistic.

## 5. Mutation-test the assertions

A passing test proves nothing about what it would catch. Break the code deliberately and confirm the
test notices.

```sh
cp internal/osc/report.go /tmp/report.bak
# apply one mutation
go build ./internal/osc/ || echo "SKIP: mutation does not compile, so it tests nothing"
go test ./internal/osc/ -count=1   # must FAIL
cp /tmp/report.bak internal/osc/report.go
```

Two rules learned the hard way:

- **Verify each mutation compiles.** A mutation that fails to build makes `go test` report a build
  failure, which reads as "the test caught it" and did not.
- **Restore from the backup copy, not from git.** Mutation testing happens on files with uncommitted
  work; `git checkout` is both the undo and the loss.

Mutate the thing the test claims to check: invert the condition you added, remove the guard, drop the
ordering. If a mutation survives, the assertion is weaker than it reads.

## 6. Watch for tests that pass for the wrong reason

Failures seen in this codebase, all of which looked like passes:

- A control that never fired, so the harness was blind and everything "passed".
- A needle written as a Go-escaped string (`\x1b` inside backticks) so it never matched real bytes.
- A pty echoing control characters in caret notation, so the expected sequence never appeared.
- A test that looped over findings and asserted only if any existed, so zero findings passed.
- A shell test that exercised a *shim* rather than the shell, because `exec.LookPath` returned a mise
  shim that could not resolve a version outside the repo.
- A control chosen so it could not answer the question: a terminal bug "did not reproduce" without cm,
  which only showed that removing the whole layer removes the bug. The real control was another
  multiplexer, and it reproduced immediately.
- A real program used as the trigger that did not send the sequence under test at all, so the run
  proved nothing while looking like a pass. Drive the pty directly instead.

The defense is a control that must fail. If you cannot make the test fail on purpose, you do not know
it can fail at all.

For anything involving escape sequences, `docs/testing.md` has the rest: which control to pick, the
four client-count states to cover, and why `cm read --raw` is not the byte stream.

## 7. Test the documented path, not a convenient one

A test that loads a shell script by sourcing it will pass while `eval "$(...)"` -- the form the docs
tell users -- is broken. That exact gap hid three bugs in cm's shell integration, including one where a
`return` inside an `eval` silently skipped the rest of a user's `.zshrc`.

Use the invocation the user is told to use.

## 8. Before saying it is fixed

- The full suite passes: `go test ./...`, then `-race` on `./internal/... ./cmd/...`.
- `mise run test-linux` if anything platform-specific, shell-related, or path-related changed.
- `git diff --stat` shows only intended changes, and no probe or backup file survives.
- The commit message says what was measured, with numbers: "0 failures in 30 runs with the drain, 6 in
  15 without" is a claim someone can check.

State what you did **not** verify. An unproven mechanism reported as understood is worse than an open
question, because nobody re-examines it.
