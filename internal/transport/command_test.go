package transport

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A proxy is any program that says the right thing and then forwards bytes, which is what makes this
// testable with no ssh, no remote host, and no cm binary. `sh -c` standing in for the far end is the same
// substitution the real transport makes: DialCommand does not know what ssh is.
func fakeProxy(t *testing.T, script string) (*commandConn, string) {
	t.Helper()

	conn, version, err := dialCommand(t.Context(), "sh", "-c", script)
	if err != nil {
		t.Fatalf("dialCommand() error = %v, want nil", err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, version
}

// The handshake completes and the connection carries bytes afterwards, which together are the whole
// contract: dialing means a server is reachable, and what follows is untouched.
func TestDialCommandHandshakeThenCarriesBytes(t *testing.T) {
	conn, version := fakeProxy(t, `printf 'cm-proxy 1 0.9.9\n'; exec cat`)

	if version != "0.9.9" {
		t.Errorf("version = %q, want %q", version, "0.9.9")
	}

	// Sent after the banner, so a byte of it appearing here would mean the handshake over-read into the
	// stream that belongs to ttrpc. That was the reason for reading the banner a byte at a time.
	want := "the bytes that follow the banner"
	if _, err := conn.Write([]byte(want)); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	got := make([]byte, len(want))
	if _, err := readFull(conn, got); err != nil {
		t.Fatalf("Read() error = %v, want nil", err)
	}
	if string(got) != want {
		t.Errorf("read %q, want %q", got, want)
	}
}

// What the program said on stderr reaches the error, because without it every way this can fail looks
// the same. A remote with no cm on its PATH is the case that produced the test: the shell answers 127
// and the only explanation is on stderr.
func TestDialCommandReportsWhatTheProgramSaid(t *testing.T) {
	_, _, err := dialCommand(t.Context(), "sh", "-c", `echo "cm: command not found" >&2; exit 127`)
	if err == nil {
		t.Fatal("dialCommand() error = nil, want an error")
	}
	for _, want := range []string{"cm: command not found", "status 127"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// An older cm answers an unknown subcommand with usage text on stdout, so the first line is not a banner
// and not an error either. The message has to quote it, or the user is told only that nothing happened.
func TestDialCommandReportsAnAnswerThatIsNotABanner(t *testing.T) {
	_, _, err := dialCommand(t.Context(), "sh", "-c", `printf 'Error: unknown command "proxy"\n'; exit 1`)
	if err == nil {
		t.Fatal("dialCommand() error = nil, want an error")
	}
	if !strings.Contains(err.Error(), `unknown command`) {
		t.Errorf("error %q does not quote what the program answered", err)
	}
}

// A protocol this build does not speak is refused by number, naming both, because "it did not work" does
// not tell anyone which end to upgrade.
func TestParseProxyBannerRejectsAnotherProtocol(t *testing.T) {
	_, err := parseProxyBanner("cm-proxy 99 0.9.9")
	if err == nil {
		t.Fatal("parseProxyBanner() error = nil, want an error")
	}
	for _, want := range []string{"99", "upgrade"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestParseProxyBanner(t *testing.T) {
	tests := []struct {
		name        string
		line        string
		wantVersion string
		wantErr     bool
	}{
		{name: "a banner", line: "cm-proxy 1 0.4.0", wantVersion: "0.4.0"},
		{name: "trailing space", line: "cm-proxy 1 0.4.0 \n", wantVersion: "0.4.0"},
		// A version is informational, so its absence is not a refusal: an older build that reported none
		// still speaks the protocol it claims.
		{name: "no version", line: "cm-proxy 1", wantVersion: ""},
		{name: "another program", line: "Last login: Tue", wantErr: true},
		{name: "no protocol number", line: "cm-proxy v1 0.4.0", wantErr: true},
		{name: "empty", line: "", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseProxyBanner(tt.line)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseProxyBanner(%q) error = %v, wantErr %v", tt.line, err, tt.wantErr)
			}
			if got != tt.wantVersion {
				t.Errorf("parseProxyBanner(%q) = %q, want %q", tt.line, got, tt.wantVersion)
			}
		})
	}
}

// Close collects the child. A connection that does not is a zombie per dial, and a client's reconnect
// loop dials once per attempt for as long as an outage lasts.
func TestCloseCollectsTheChild(t *testing.T) {
	conn, _ := fakeProxy(t, `printf 'cm-proxy 1 0.9.9\n'; exec cat`)

	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if conn.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil after Close, so the child was never waited for")
	}
}

// A child that ignores the EOF on its stdin is killed rather than waited on forever, because the case
// this covers is an ssh whose far end has gone, which is exactly when a connection gets closed.
func TestCloseKillsAChildThatWillNotExit(t *testing.T) {
	// Traps the signal a closing stdin does not send, and reads nothing, so EOF on stdin moves it not at
	// all. Only a kill ends it.
	conn, _ := fakeProxy(t, `printf 'cm-proxy 1 0.9.9\n'; trap '' HUP PIPE; while :; do sleep 10; done`)

	// A grace of reapGrace has to elapse first, so the bound being honored is part of what is asserted:
	// this returning promptly would mean the kill happened without waiting for the child to go on its own.
	began := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	if waited := time.Since(began); waited < reapGrace {
		t.Errorf("Close() returned after %v, want at least the %v grace", waited, reapGrace)
	}
	if conn.cmd.ProcessState == nil {
		t.Fatal("ProcessState is nil after Close, so the child was never collected")
	}
}

// Close does not wait for a process that merely inherited the child's stderr.
//
// The measurement that produced this: with cmd.Stderr set to an io.Writer, os/exec makes a copying
// goroutine and Wait waits for it, which ends only when every holder of the write end has let go. A
// grandchild with eight seconds left to live made Close take 8.01s after the child was already dead. An
// ssh with a ProxyCommand or a ProxyJump has exactly this shape, so this is the ordinary case rather than
// a contrived one.
func TestCloseDoesNotWaitForAProcessHoldingStderr(t *testing.T) {
	// The background sleep inherits stderr and outlives cat by a long way. cat exits as soon as its stdin
	// closes, so a prompt Close is the assertion and the sleep is what could hold it up.
	const grandchildLife = 30 * time.Second
	conn, _ := fakeProxy(t, `printf 'cm-proxy 1 0.9.9\n'; sleep 30 & exec cat`)

	began := time.Now()
	if err := conn.Close(); err != nil {
		t.Fatalf("Close() error = %v, want nil", err)
	}
	// Generously below the grandchild's life and above anything a local exit costs, so this fails on the
	// bug rather than on a slow machine.
	if waited := time.Since(began); waited > grandchildLife/4 {
		t.Errorf("Close() took %v, so it waited for a process that only inherited stderr", waited)
	}
}

// Close twice is not an error, because a caller that closes a connection it has already handed to ttrpc
// has no way to know whether ttrpc got there first.
func TestCloseTwice(t *testing.T) {
	conn, _ := fakeProxy(t, `printf 'cm-proxy 1 0.9.9\n'; exec cat`)

	first := conn.Close()
	second := conn.Close()
	if first != nil || second != nil {
		t.Errorf("Close() twice = %v then %v, want nil both times", first, second)
	}
}

// Deadlines are real rather than refused, which is what holding *os.File on both halves buys. ttrpc does
// not set one today, and a net.Conn that quietly ignored them would be a trap for whatever does.
func TestReadDeadline(t *testing.T) {
	conn, _ := fakeProxy(t, `printf 'cm-proxy 1 0.9.9\n'; exec cat`)

	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v, want nil", err)
	}
	_, err := conn.Read(make([]byte, 1))
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("Read() after the deadline = %v, want %v", err, os.ErrDeadlineExceeded)
	}
}

// The banner wait is bounded, so a program that connects and then says nothing fails instead of hanging.
// Asserted through a cancelled context rather than by waiting out proxyReadyTimeout, which is thirty
// seconds by design.
func TestDialCommandGivesUpWhenTheContextDoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	began := time.Now()
	_, _, err := dialCommand(ctx, "sh", "-c", `while :; do sleep 10; done`)
	if err == nil {
		t.Fatal("dialCommand() error = nil, want an error")
	}
	if waited := time.Since(began); waited > proxyReadyTimeout {
		t.Errorf("dialCommand() waited %v, which is past the context's own deadline", waited)
	}
}

// The tail is kept rather than the head, since the last thing a program says is why it failed, and it is
// bounded, since the program lives as long as the connection.
func TestTailBufferKeepsTheEnd(t *testing.T) {
	b := &tailBuffer{}
	if _, err := b.Write([]byte(strings.Repeat("x", diagTail))); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}
	if _, err := b.Write([]byte("\nthe last thing it said\n")); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}

	got := b.String()
	if len(got) > diagTail {
		t.Errorf("String() is %d bytes, over the %d-byte bound", len(got), diagTail)
	}
	if !strings.HasSuffix(got, "the last thing it said") {
		t.Errorf("String() = %q, want it to end with what was written last", got)
	}
}

// Several lines become one, because this goes into an error message and ssh answers a bad host with
// three.
func TestTailBufferJoinsLines(t *testing.T) {
	b := &tailBuffer{}
	if _, err := b.Write([]byte("ssh: Could not resolve hostname nope\n\nlost connection\n")); err != nil {
		t.Fatalf("Write() error = %v, want nil", err)
	}

	got := b.String()
	want := "ssh: Could not resolve hostname nope; lost connection"
	if got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// readFull is io.ReadFull, spelled out to keep the read path in the test explicit about what it waits for.
func readFull(conn *commandConn, p []byte) (int, error) {
	read := 0
	for read < len(p) {
		n, err := conn.Read(p[read:])
		read += n
		if err != nil {
			return read, err
		}
	}
	return read, nil
}

// The message ends on the reason rather than on the word EOF, which is what the pipe reports and says
// nothing: "ssh: Could not resolve hostname work: EOF" ends on its least informative part.
func TestStartupErrorEndsOnTheReason(t *testing.T) {
	_, _, err := dialCommand(t.Context(), "sh", "-c", `echo "could not resolve hostname work" >&2; exit 255`)
	if err == nil {
		t.Fatal("dialCommand() error = nil, want an error")
	}
	if !strings.HasSuffix(err.Error(), "could not resolve hostname work") {
		t.Errorf("error = %q, want it to end on what the program said", err)
	}
}
