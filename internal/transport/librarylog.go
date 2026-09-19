package transport

import (
	"context"
	"io"
	"log/slog"
	"slices"
	"sync/atomic"

	"github.com/sirupsen/logrus"
)

// ttrpc logs through containerd/log, which is logrus's standard logger, whose output is os.Stderr.
// For an attached client that is the user's terminal, so a stream ttrpc gave up on printed
// `level=error msg="ttrpc: failed to handle message" error="ttrpc: stream buffer full"` into the
// middle of a session. Written directly, bypassing internal/client.screen, so it can land inside a
// sequence the program was halfway through: the same failure a window title written to os.Stdout
// caused. Nothing about it reached any cm log either, so the corrupted terminal was the only
// evidence the incident left.
//
// Done in init rather than from each entry point, and both halves of that are deliberate. Importing
// this package is what makes ttrpc reachable, so there is no way to use ttrpc in this codebase and
// not be covered. And the client builds no logger at all when logging is disabled, which is the
// configuration this was reported from: anything hung off a logger would have missed exactly the
// case that matters.
func init() {
	std := logrus.StandardLogger()
	std.SetOutput(io.Discard)
	std.AddHook(libraryHook{})
	// Everything is forwarded and cm's handler decides what to keep, since a level set here would
	// discard a line before the destination that cares about it ever saw it.
	std.SetLevel(logrus.TraceLevel)
}

// libraryLog is where a forwarded line goes. Nil until LogTo, which discards: silence is the right
// default for a process holding a terminal, and a lost diagnostic beats a corrupted screen.
var libraryLog atomic.Pointer[slog.Logger]

// LogTo sends what a library logs into cm's own log.
//
// Worth wiring up wherever there is a logger: "ttrpc: stream buffer full" in server.log names the
// fault immediately, and its absence is what made the same failure take a day to place.
func LogTo(l *slog.Logger) {
	if l == nil {
		return
	}
	libraryLog.Store(l)
}

// libraryHook forwards logrus records to cm's logger.
type libraryHook struct{}

func (libraryHook) Levels() []logrus.Level { return logrus.AllLevels }

func (libraryHook) Fire(e *logrus.Entry) error {
	l := libraryLog.Load()
	if l == nil {
		return nil
	}
	// Sorted, so a record with several fields reads the same way twice. ttrpc's carry an error and a
	// stream id.
	keys := make([]string, 0, len(e.Data))
	for k := range e.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	attrs := make([]any, 0, len(keys)*2)
	for _, k := range keys {
		attrs = append(attrs, k, e.Data[k])
	}
	l.Log(context.Background(), slogLevel(e.Level), e.Message, attrs...)
	return nil
}

func slogLevel(l logrus.Level) slog.Level {
	switch l {
	case logrus.PanicLevel, logrus.FatalLevel, logrus.ErrorLevel:
		return slog.LevelError
	case logrus.WarnLevel:
		return slog.LevelWarn
	case logrus.InfoLevel:
		return slog.LevelInfo
	default:
		return slog.LevelDebug
	}
}
