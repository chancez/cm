package transport

import (
	"bytes"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/containerd/log"
	"github.com/sirupsen/logrus"
)

// TestALibraryNeverWritesToStderr is the guard for the bug this package's init exists for: ttrpc
// printed "ttrpc: stream buffer full" into an attached client's terminal, because containerd/log is
// logrus's standard logger and its output is os.Stderr.
func TestALibraryNeverWritesToStderr(t *testing.T) {
	if out := logrus.StandardLogger().Out; out != io.Discard {
		t.Errorf("logrus standard output = %#v, want io.Discard", out)
	}
}

// TestALibraryLogsIntoCMsLog checks the other half: the line is not merely silenced, it is recorded
// where a person diagnosing the session can find it. Its absence from every cm log is what made the
// original failure take a day to place.
func TestALibraryLogsIntoCMsLog(t *testing.T) {
	var buf bytes.Buffer
	LogTo(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { libraryLog.Store(nil) })

	log.L.WithFields(log.Fields{"error": "ttrpc: stream buffer full", "stream": 1}).
		Error("ttrpc: failed to handle message")

	got := buf.String()
	for _, want := range []string{
		`level=ERROR`,
		`msg="ttrpc: failed to handle message"`,
		`error="ttrpc: stream buffer full"`,
		`stream=1`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("forwarded record = %q, missing %q", got, want)
		}
	}
}

func TestLogrusLevelsMapToSlog(t *testing.T) {
	// Every logrus level, so a level added upstream shows up here as a missing case rather than as
	// silently becoming Debug.
	want := map[logrus.Level]slog.Level{
		logrus.PanicLevel: slog.LevelError,
		logrus.FatalLevel: slog.LevelError,
		logrus.ErrorLevel: slog.LevelError,
		logrus.WarnLevel:  slog.LevelWarn,
		logrus.InfoLevel:  slog.LevelInfo,
		logrus.DebugLevel: slog.LevelDebug,
		logrus.TraceLevel: slog.LevelDebug,
	}
	got := make(map[logrus.Level]slog.Level, len(logrus.AllLevels))
	for _, l := range logrus.AllLevels {
		got[l] = slogLevel(l)
	}
	if len(got) != len(want) {
		t.Fatalf("levels = %v, want %v", got, want)
	}
	for l, w := range want {
		if got[l] != w {
			t.Errorf("slogLevel(%v) = %v, want %v", l, got[l], w)
		}
	}
}

// TestLogToNilKeepsTheCurrentDestination covers the caller that has no logger, which is a client
// with logging disabled. Discarding is right there; replacing a logger already set with nil is not.
func TestLogToNilKeepsTheCurrentDestination(t *testing.T) {
	var buf bytes.Buffer
	l := slog.New(slog.NewTextHandler(&buf, nil))
	LogTo(l)
	t.Cleanup(func() { libraryLog.Store(nil) })

	LogTo(nil)

	if got := libraryLog.Load(); got != l {
		t.Errorf("destination after LogTo(nil) = %v, want it unchanged", got)
	}
}
