package cmlog

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name    string
		level   slog.Level
		enabled bool
		wantErr bool
	}{
		// Unset means on at info, since the log is what makes a detached server diagnosable and
		// defaulting to silence would defeat that.
		{name: "", level: slog.LevelInfo, enabled: true},
		{name: "info", level: slog.LevelInfo, enabled: true},
		{name: "debug", level: slog.LevelDebug, enabled: true},
		{name: "warn", level: slog.LevelWarn, enabled: true},
		{name: "error", level: slog.LevelError, enabled: true},
		{name: "off", enabled: false},
		{name: "INFO", level: slog.LevelInfo, enabled: true},
		{name: "  debug  ", level: slog.LevelDebug, enabled: true},
		{name: "verbose", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, enabled, err := ParseLevel(tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseLevel(%q) error = %v, wantErr %v", tt.name, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if enabled != tt.enabled {
				t.Errorf("enabled = %v, want %v", enabled, tt.enabled)
			}
			if enabled && level != tt.level {
				t.Errorf("level = %v, want %v", level, tt.level)
			}
		})
	}
}

// A disabled logger must still be usable, or every call site would need a nil check and some would
// forget.
func TestNewDisabledReturnsUsableLogger(t *testing.T) {
	logger, closer, err := New(Options{Enabled: false})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer closer.Close()
	if logger == nil {
		t.Fatal("New() returned a nil logger with logging disabled")
	}
	// Must not panic.
	logger.Info("nothing", "key", "value")
	logger.Error("nothing", "key", "value")
}

func TestNewWritesToFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "server.log")

	logger, closer, err := New(Options{Path: path, Level: slog.LevelInfo, Enabled: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger.Info("hello", "session", "work")
	if err := closer.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	got := string(data)
	for _, want := range []string{"hello", "session=work", "level=INFO"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q missing %q", got, want)
		}
	}
}

// The log records session names, directories, and command lines, so it must not be world-readable.
func TestLogFileIsOwnerOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600", perm)
	}
}

// A level below the threshold must not be written, or a debug-heavy build would fill the file.
func TestLevelFiltering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	logger, closer, err := New(Options{Path: path, Level: slog.LevelWarn, Enabled: true})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	logger.Debug("debug message")
	logger.Info("info message")
	logger.Warn("warn message")
	closer.Close()

	data, _ := os.ReadFile(path)
	got := string(data)
	if strings.Contains(got, "debug message") || strings.Contains(got, "info message") {
		t.Errorf("log contains messages below the threshold: %q", got)
	}
	if !strings.Contains(got, "warn message") {
		t.Errorf("log is missing the warning: %q", got)
	}
}

// Rotation bounds the file, since a long-lived server would otherwise fill the disk slowly enough
// that nobody notices until it matters.
func TestRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	defer f.Close()

	chunk := []byte(strings.Repeat("x", 64*1024) + "\n")
	for written := 0; written < MaxBytes+len(chunk); written += len(chunk) {
		if _, err := f.Write(chunk); err != nil {
			t.Fatalf("Write() error = %v", err)
		}
	}

	current, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if current.Size() > MaxBytes {
		t.Errorf("current log is %d bytes, want at most %d", current.Size(), MaxBytes)
	}
	// The previous generation must be kept, since the interesting event is usually just before the
	// rotation.
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Errorf("rotated log missing: %v", err)
	}
}

// Writing after close must not fail, or shutdown ordering would produce spurious errors in code
// whose only job is logging.
func TestWriteAfterCloseSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	f, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := f.Write([]byte("after close\n")); err != nil {
		t.Errorf("Write() after Close error = %v, want nil", err)
	}
	if err := f.Close(); err != nil {
		t.Errorf("second Close() error = %v, want nil", err)
	}
}

// An existing log is appended to rather than truncated, so a restart does not discard the record of
// why the previous run ended.
func TestOpenFileAppends(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")

	first, err := OpenFile(path)
	if err != nil {
		t.Fatalf("OpenFile() error = %v", err)
	}
	first.Write([]byte("before restart\n"))
	first.Close()

	second, err := OpenFile(path)
	if err != nil {
		t.Fatalf("reopen error = %v", err)
	}
	second.Write([]byte("after restart\n"))
	second.Close()

	data, _ := os.ReadFile(path)
	for _, want := range []string{"before restart", "after restart"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("log %q missing %q", data, want)
		}
	}
}
