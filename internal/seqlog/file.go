package seqlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/chancez/cm/internal/seq"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// FileLimits bounds what a persisted log retains.
type FileLimits struct {
	// MaxLines is the number of lines to keep, counted by newlines. Zero means unlimited.
	MaxLines int
	// MaxBytes is a ceiling that applies regardless of MaxLines. Zero means unlimited.
	//
	// Necessary as a backstop rather than an alternative: the log holds raw pty bytes and has no
	// concept of a line beyond counting newlines, so a session emitting very long lines would
	// otherwise produce a file far larger than its line count suggests. One pathological line must
	// not be able to fill the disk.
	MaxBytes int64
}

// DefaultFileLimits retains enough to rebuild a screen and a useful amount of scrollback.
var DefaultFileLimits = FileLimits{
	MaxLines: 10000,
	MaxBytes: 16 << 20, // 16 MiB
}

// fileHeader precedes the payload and records where the retained bytes start in the session's
// sequence numbering.
//
// Necessary because trimming drops bytes from the front, so a file's first byte is not sequence
// zero. Without recording the offset, a server replaying the file after a reboot would number the
// output from zero and disagree with everything else about where it is.
const (
	fileMagic       = "cmlog1\n"
	fileHeaderBytes = len(fileMagic) + 8 // magic plus a big-endian uint64 oldest sequence
)

// File is an append-only log on disk, bounded and trimmed from the front.
//
// It exists so a session's content survives more than the server: killing the shim, or rebooting,
// leaves the file, and replaying it through a terminal emulator rebuilds the screen.
//
// Writes are not synced. Terminal output is worth keeping but not worth an fsync per chunk, and the
// failure this protects against is a process dying rather than the machine losing power mid-write.
type File[S seq.Number] struct {
	mu     sync.Mutex
	f      *os.File
	limits FileLimits

	// oldest is the sequence number of the first retained byte.
	oldest S
	// size is the payload length, excluding the header.
	size int64
	// lines counts newlines in the payload, so trimming does not have to rescan the file.
	lines int
}

// OpenFile opens or creates a persisted log.
//
// An existing file is adopted rather than truncated, which is what makes the log survive a restart:
// the shim reopens it and continues appending at the recorded position.
func OpenFile[S seq.Number](path string, limits FileLimits) (*File[S], error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}

	// Owner-only: a session's output routinely contains whatever the user typed or read.
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening log %s: %w", path, err)
	}

	fl := &File[S]{f: f, limits: limits}
	if err := fl.load(); err != nil {
		f.Close()
		return nil, err
	}
	return fl, nil
}

// load reads the header and measures the payload, writing a header if the file is new.
//
// A file whose header is missing or unrecognized is truncated rather than rejected. The alternative
// is refusing to start a session because of an unreadable cache, which trades something recoverable
// for something that is not.
func (l *File[S]) load() error {
	info, err := l.f.Stat()
	if err != nil {
		return fmt.Errorf("measuring log: %w", err)
	}

	if info.Size() < int64(fileHeaderBytes) {
		return l.reset(0)
	}

	header := make([]byte, fileHeaderBytes)
	if _, err := l.f.ReadAt(header, 0); err != nil {
		return fmt.Errorf("reading log header: %w", err)
	}
	if !bytes.Equal(header[:len(fileMagic)], []byte(fileMagic)) {
		return l.reset(0)
	}

	l.oldest = S(binary.BigEndian.Uint64(header[len(fileMagic):]))
	l.size = info.Size() - int64(fileHeaderBytes)
	l.lines, err = countLines(l.f, int64(fileHeaderBytes), l.size)
	if err != nil {
		return err
	}
	return nil
}

// reset truncates the file and writes a header numbering the payload from oldest.
func (l *File[S]) reset(oldest S) error {
	if err := l.f.Truncate(0); err != nil {
		return fmt.Errorf("truncating log: %w", err)
	}
	header := make([]byte, fileHeaderBytes)
	copy(header, fileMagic)
	binary.BigEndian.PutUint64(header[len(fileMagic):], uint64(oldest))
	if _, err := l.f.WriteAt(header, 0); err != nil {
		return fmt.Errorf("writing log header: %w", err)
	}
	l.oldest, l.size, l.lines = oldest, 0, 0
	return nil
}

// countLines counts newlines in a region of the file.
func countLines(r io.ReaderAt, off, size int64) (int, error) {
	const chunk = 64 << 10
	buf := make([]byte, chunk)

	n := 0
	for read := int64(0); read < size; {
		want := min(int64(chunk), size-read)
		got, err := r.ReadAt(buf[:want], off+read)
		if got > 0 {
			n += bytes.Count(buf[:got], []byte{'\n'})
			read += int64(got)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return n, fmt.Errorf("scanning log: %w", err)
		}
	}
	return n, nil
}

// Append adds output and trims the front if the limits are exceeded.
func (l *File[S]) Append(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if _, err := l.f.WriteAt(p, int64(fileHeaderBytes)+l.size); err != nil {
		return fmt.Errorf("appending to log: %w", err)
	}
	l.size += int64(len(p))
	l.lines += bytes.Count(p, []byte{'\n'})

	return l.trimLocked()
}

// trimLocked drops leading bytes until the log is within its limits.
//
// Trimming happens on a line boundary when a line limit is what was exceeded, so the retained
// content starts at the beginning of a line rather than mid-escape. When the byte ceiling is what
// was exceeded, an exact boundary is not available and the cut is made anyway: a caller replaying
// from the middle of a sequence is told there is a gap, which is the same contract the in-memory log
// has.
func (l *File[S]) trimLocked() error {
	drop := int64(0)

	if l.limits.MaxLines > 0 && l.lines > l.limits.MaxLines {
		off, err := l.offsetAfterLines(l.lines - l.limits.MaxLines)
		if err != nil {
			return err
		}
		drop = off
	}
	if l.limits.MaxBytes > 0 && l.size-drop > l.limits.MaxBytes {
		drop = l.size - l.limits.MaxBytes
	}
	if drop <= 0 {
		return nil
	}

	// Rewrite the file without the dropped prefix.
	//
	// A copy rather than a hole-punch or a ring: the file has to stay a plain readable log so a
	// future cm, or a person with `cat`, can make sense of it, and trimming is rare enough that
	// rewriting is cheaper to get right than an in-place scheme.
	remaining := l.size - drop
	buf := make([]byte, remaining)
	if remaining > 0 {
		if _, err := l.f.ReadAt(buf, int64(fileHeaderBytes)+drop); err != nil {
			return fmt.Errorf("reading log during trim: %w", err)
		}
	}

	newOldest := l.oldest + S(drop)
	if err := l.reset(newOldest); err != nil {
		return err
	}
	if remaining > 0 {
		if _, err := l.f.WriteAt(buf, int64(fileHeaderBytes)); err != nil {
			return fmt.Errorf("rewriting log during trim: %w", err)
		}
		l.size = remaining
		l.lines = bytes.Count(buf, []byte{'\n'})
	}
	return nil
}

// offsetAfterLines returns the payload offset just past the nth newline.
func (l *File[S]) offsetAfterLines(n int) (int64, error) {
	if n <= 0 {
		return 0, nil
	}

	const chunk = 64 << 10
	buf := make([]byte, chunk)

	seen := 0
	for read := int64(0); read < l.size; {
		want := min(int64(chunk), l.size-read)
		got, err := l.f.ReadAt(buf[:want], int64(fileHeaderBytes)+read)
		for i := 0; i < got; i++ {
			if buf[i] == '\n' {
				seen++
				if seen == n {
					return read + int64(i) + 1, nil
				}
			}
		}
		read += int64(got)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return 0, fmt.Errorf("scanning log: %w", err)
		}
	}
	// Fewer newlines than asked for, so everything is droppable.
	return l.size, nil
}

// Bounds returns the oldest retained sequence number and the next to be assigned.
func (l *File[S]) Bounds() (oldest, next S) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oldest, l.oldest + S(l.size)
}

// ReadFrom returns retained bytes starting at a sequence number, and whether bytes before it were
// already dropped.
//
// A gap is reported rather than an error because it is expected: a caller resuming from a position
// the log has since trimmed needs to know its view is discontinuous, not that something failed.
func (l *File[S]) ReadFrom(from S) (data []byte, gap bool, err error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	start := from
	if start < l.oldest {
		start, gap = l.oldest, true
	}
	end := l.oldest + S(l.size)
	if start >= end {
		return nil, gap, nil
	}

	off := int64(start - l.oldest)
	out := make([]byte, l.size-off)
	if _, err := l.f.ReadAt(out, int64(fileHeaderBytes)+off); err != nil && !errors.Is(err, io.EOF) {
		return nil, gap, fmt.Errorf("reading log: %w", err)
	}
	return out, gap, nil
}

// Sync flushes the file to disk.
//
// Called at points where losing the tail would matter, such as a session ending, rather than on
// every append: terminal output is worth keeping but not worth an fsync per chunk.
func (l *File[S]) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f.Sync()
}

// Close flushes and releases the file.
func (l *File[S]) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Sync()
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
