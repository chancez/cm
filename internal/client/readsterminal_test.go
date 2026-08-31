package client

import (
	"io"
	"os"
	"testing"
)

// Which clients read the terminal, which is what decides whether a reader is built at all.
//
// The guard has to be here rather than on the failure it prevents. A reader that cannot be created is a
// Linux-only error: cancelreader registers the descriptor with epoll, which accepts a pipe or a tty and
// refuses a regular file or /dev/null, while darwin's is select-based and accepts all of them. Measured in
// the Linux test image, calling cancelreader.NewReader directly: pipe returned nil, and a redirected
// stdin, /dev/null and a regular file each returned "add reader to epoll interest list". So a test that
// asserted the error would pass on darwin with the bug present, which is the shape AGENTS.md warns about.
//
// What the bug was: `cm read --follow` and `cm send --follow` exited 1 on Linux whenever stdin was
// redirected, which is every script, cron job and CI run, because Attach built a reader before asking
// whether this client reads keys.
func TestReadsTerminal(t *testing.T) {
	// A tty that is not a terminal, which is what every one of these commands has when its output is
	// piped. The file makes IsTerminal false without needing a pty.
	notATerminal := ttyForTest(t)

	detachOff := KeySpec{Name: "none", Disabled: true}
	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey() error = %v", err)
	}

	tests := []struct {
		name string
		opts Options
		want bool
	}{
		{
			// followSession's options: cm read --follow, cm send --follow, cm run's streaming.
			name: "a follower streaming to a pipe",
			opts: Options{ReadOnly: true, DetachKey: detachOff, NoRestore: true, Output: io.Discard},
			want: false,
		},
		{
			// The case that rules out keying off ReadOnly alone: interactive, so it still needs the key
			// that ends it.
			name: "cm attach --read-only",
			opts: Options{ReadOnly: true, PrefixKey: prefix},
			want: true,
		},
		{
			name: "an ordinary attach",
			opts: Options{PrefixKey: prefix},
			want: true,
		},
		{
			// Nothing configures this today. It is here because a zero DetachKey means the default key
			// rather than none, and reading it as none would silently stop an attachment being escapable.
			name: "read-only with the detach key left at its default",
			opts: Options{ReadOnly: true},
			want: true,
		},
		{
			// A prefix key with nowhere to paint buys nothing, so it does not earn a reader either.
			name: "a follower that was handed a prefix key anyway",
			opts: Options{
				ReadOnly: true, DetachKey: detachOff, NoRestore: true,
				Output: io.Discard, PrefixKey: prefix,
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.readsTerminal(notATerminal); got != tt.want {
				t.Errorf("readsTerminal() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestAnIdleInputNeverDelivers pins what a client with no reader gets instead.
//
// Not a nil input, because the attachment selects on these channels either way: the alternative was a nil
// check at every one of those selects.
func TestAnIdleInputNeverDelivers(t *testing.T) {
	in := newIdleInput()

	select {
	case data := <-in.data:
		t.Errorf("an idle input delivered %q, want nothing", data)
	case err := <-in.errs:
		t.Errorf("an idle input reported %v, want nothing", err)
	default:
	}

	// Both no-ops rather than panics, which is what lets Attach defer the suspend unconditionally.
	if err := in.resume(); err != nil {
		t.Errorf("resume() on an idle input error = %v", err)
	}
	in.suspend()
}

// ttyForTest returns a TTY over a regular file, so IsTerminal is false.
//
// A file rather than a pipe deliberately: it is the shape that broke on Linux, and OpenTTYCooked on it is
// what a piped `cm read --follow` does.
func ttyForTest(t *testing.T) *TTY {
	t.Helper()

	f, err := os.CreateTemp("", "cmtty")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	t.Cleanup(func() {
		f.Close()
		os.Remove(f.Name())
	})

	tty, err := OpenTTYCooked(f, f)
	if err != nil {
		t.Fatalf("OpenTTYCooked() error = %v", err)
	}
	t.Cleanup(func() { tty.Close() })
	return tty
}
