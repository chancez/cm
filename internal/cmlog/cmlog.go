// Package cmlog writes cm's diagnostic log.
//
// The server and shim run detached with their stdio discarded, which is deliberate: inheriting a
// client's terminal would tie their lifetime to a window and scribble over the session. The
// consequence is that without a log there is no record of anything they do, and several errors are
// deliberately swallowed so a failure in something advisory, like writing a title to the database,
// cannot end a session. Those two facts together make a system that degrades silently and cannot
// say so.
//
// So the rule this package exists to serve: an error that is swallowed must still be logged.
package cmlog

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Level names accepted in configuration.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
	LevelOff   = "off"
)

// ParseLevel resolves a configured level name.
func ParseLevel(name string) (slog.Level, bool, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", LevelInfo:
		return slog.LevelInfo, true, nil
	case LevelDebug:
		return slog.LevelDebug, true, nil
	case LevelWarn:
		return slog.LevelWarn, true, nil
	case LevelError:
		return slog.LevelError, true, nil
	case LevelOff:
		return slog.LevelInfo, false, nil
	default:
		return slog.LevelInfo, false, fmt.Errorf(
			"log level %q, want debug, info, warn, error, or off", name)
	}
}

// MaxBytes is the size at which a log file is rotated.
//
// Rotation rather than unbounded growth, because a long-lived server logging every session's
// lifecycle would otherwise fill the disk slowly enough that nobody notices until it matters.
const MaxBytes = 4 << 20

// File is a size-bounded log file.
//
// One previous generation is kept, which is enough to cover the case this exists for: something went
// wrong a moment ago and the current file has already rotated past it.
type File struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

// OpenFile opens or creates a log file, appending to an existing one.
func OpenFile(path string) (*File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}
	// Owner-only: a log records session names, working directories, and command lines.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening log %s: %w", path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("measuring log: %w", err)
	}
	return &File{path: path, f: f, size: info.Size()}, nil
}

// Write appends to the log, rotating first when it would exceed the limit.
func (l *File) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.f == nil {
		// Closed. Reporting success rather than an error, because a logger that fails writes would
		// turn shutdown ordering into a source of spurious errors in code that only logs.
		return len(p), nil
	}

	if l.size+int64(len(p)) > MaxBytes {
		if err := l.rotateLocked(); err != nil {
			// Rotation failing must not stop logging: appending to an oversized file is better than
			// losing the message that probably explains why rotation failed.
			_ = err
		}
	}

	n, err := l.f.Write(p)
	l.size += int64(n)
	return n, err
}

// rotateLocked moves the current file aside and starts a new one.
func (l *File) rotateLocked() error {
	if err := l.f.Close(); err != nil {
		return err
	}
	// A single previous generation, overwritten. Keeping more would need a retention policy for
	// something that only matters for the last few minutes.
	if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	l.f, l.size = f, 0
	return nil
}

// Close releases the file.
func (l *File) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// Options configures a logger.
type Options struct {
	// Path is the file to write to. Empty writes to Stderr instead, which is what a foreground
	// server wants.
	Path string
	// Level is the minimum severity to record.
	Level slog.Level
	// Enabled turns logging on. When false, a logger that discards everything is returned, so
	// callers never have to check.
	Enabled bool
	// Stderr also writes to standard error, for a server run in the foreground.
	Stderr bool
}

// New builds a logger and a closer for whatever it opened.
//
// Never returns a nil logger: a caller that has to nil-check before every log line ends up not
// logging, which defeats the purpose. Disabled logging is a discarding handler instead.
func New(opts Options) (*slog.Logger, io.Closer, error) {
	if !opts.Enabled {
		return slog.New(discardHandler{}), noopCloser{}, nil
	}

	var (
		w      io.Writer = os.Stderr
		closer io.Closer = noopCloser{}
	)
	if opts.Path != "" {
		f, err := OpenFile(opts.Path)
		if err != nil {
			return nil, nil, err
		}
		closer = f
		if opts.Stderr {
			w = io.MultiWriter(f, os.Stderr)
		} else {
			w = f
		}
	}

	// Text rather than JSON: this is read by a person diagnosing something, usually with grep, and
	// the structure is shallow enough that keys and values on one line are easier to scan.
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: opts.Level})
	return slog.New(h), closer, nil
}

// Discard returns a logger that records nothing, for tests and for callers with no logger.
func Discard() *slog.Logger { return slog.New(discardHandler{}) }

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }

type noopCloser struct{}

func (noopCloser) Close() error { return nil }
