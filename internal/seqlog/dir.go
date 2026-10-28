package seqlog

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"sync"

	"github.com/chancez/cm/internal/seq"
)

// DefaultSegmentBytes is the size at which a Dir seals its active segment and starts another.
//
// Small enough that retention, which only drops whole segments, overshoots its limit by little: 1 MiB
// against the default 16 MiB ceiling. Large enough that a busy session rotates rarely, since a rotation
// opens a file and lists none.
const DefaultSegmentBytes = 1 << 20

// segmentPattern matches a segment's name: the sequence number of its first byte, zero-padded to the
// width of a uint64 so that sorting names sorts positions.
var segmentPattern = regexp.MustCompile(`^[0-9]{20}\.seg$`)

func segmentName(start uint64) string { return fmt.Sprintf("%020d.seg", start) }

// Persisted is what a persisted log offers, whichever layout holds it.
type Persisted[S seq.Number] interface {
	Append(p []byte) error
	Bounds() (oldest, next S)
	ReadFrom(from S) (data []byte, gap bool, err error)
	Sync() error
	Close() error
}

var (
	_ Persisted[seq.Shim] = (*File[seq.Shim])(nil)
	_ Persisted[seq.Shim] = (*Dir[seq.Shim])(nil)
)

// OpenPersisted opens a log at path in whichever layout it already has: a directory of segments, or
// the single file an older shim writes.
//
// For readers. A session adopted from an older shim keeps writing a single file for as long as that
// shim lives, so the server meets both layouts at the same path column.
func OpenPersisted[S seq.Number](path string, limits FileLimits) (Persisted[S], error) {
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		return OpenDir[S](path, limits)
	}
	return OpenFile[S](path, limits)
}

type segment struct {
	start uint64
	size  int64
	lines int
}

// Dir is a persisted log kept as a directory of segment files, retained by deleting whole segments.
//
// It replaces File's trim, which rewrote everything retained to drop a prefix. Once a log reached its
// cap that ran on every append, on the shim's pty read loop: 4.1ms per 1022-byte read at the 16 MiB
// default, so a program redrawing a large screen stalled behind the disk. nvim scrolling a vertical
// split under cm was the report. Here an append is a write to the end of one file whatever is retained.
//
// Each segment holds raw output and nothing else, and its name is the sequence number of its first
// byte, so `cat *` in name order is the log and the directory listing is the index. No manifest: it
// would be a second record that can disagree with the files.
type Dir[S seq.Number] struct {
	mu      sync.Mutex
	path    string
	limits  FileLimits
	segSize int64

	// segs is ordered by start and contiguous; the last is the one being written.
	segs []segment
	// active is the open last segment, or nil before the first append.
	active *os.File
	// size and lines total every segment.
	size  int64
	lines int
}

// OpenDir opens or creates a segmented log.
//
// Opening changes nothing on disk beyond creating the directory, so a reader can open the log of a
// session whose shim is still writing it.
func OpenDir[S seq.Number](path string, limits FileLimits) (*Dir[S], error) {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return nil, fmt.Errorf("creating log directory: %w", err)
	}
	d := &Dir[S]{path: path, limits: limits, segSize: limits.SegmentBytes}
	if d.segSize <= 0 {
		d.segSize = DefaultSegmentBytes
	}
	if err := d.load(); err != nil {
		return nil, err
	}
	return d, nil
}

// load lists the segments and measures them.
//
// A break in the numbering keeps only what follows it. Rotation creates the next segment at exactly
// the previous end, so a break means a file was removed by something other than cm, and the bytes
// before it cannot be given positions that agree with the bytes after.
func (d *Dir[S]) load() error {
	entries, err := os.ReadDir(d.path)
	if err != nil {
		return fmt.Errorf("listing log: %w", err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !segmentPattern.MatchString(e.Name()) {
			continue
		}
		start, err := strconv.ParseUint(e.Name()[:20], 10, 64)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			return fmt.Errorf("measuring log segment: %w", err)
		}
		d.segs = append(d.segs, segment{start: start, size: info.Size()})
	}
	slices.SortFunc(d.segs, func(a, b segment) int { return cmp.Compare(a.start, b.start) })

	for i := len(d.segs) - 1; i > 0; i-- {
		if d.segs[i-1].start+uint64(d.segs[i-1].size) != d.segs[i].start {
			d.segs = d.segs[i:]
			break
		}
	}

	for i := range d.segs {
		f, err := os.Open(d.segPath(d.segs[i].start))
		if err != nil {
			return fmt.Errorf("opening log segment: %w", err)
		}
		d.segs[i].lines, err = countLines(f, 0, d.segs[i].size)
		f.Close()
		if err != nil {
			return err
		}
		d.size += d.segs[i].size
		d.lines += d.segs[i].lines
	}
	return nil
}

func (d *Dir[S]) segPath(start uint64) string { return filepath.Join(d.path, segmentName(start)) }

// end is the sequence number the next byte will have.
func (d *Dir[S]) end() uint64 {
	if len(d.segs) == 0 {
		return 0
	}
	last := d.segs[len(d.segs)-1]
	return last.start + uint64(last.size)
}

// Append adds output, sealing the active segment once it passes the segment size.
//
// A segment is sealed at the first newline past the size, so retained content starts at the beginning
// of a line rather than mid-escape, the property File's line trim was written for. Output with no
// newline, which is most of what a full-screen program writes, is cut at twice the size anyway: one
// redraw must not grow a segment without bound, and a reader starting mid-sequence is told there is a
// gap, the same contract as File's byte ceiling.
func (d *Dir[S]) Append(p []byte) error {
	if len(p) == 0 {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.active == nil {
		if err := d.openActive(); err != nil {
			return err
		}
	}
	for len(p) > 0 {
		cur := d.segs[len(d.segs)-1].size
		if cur >= 2*d.segSize {
			// Only reachable by reopening with a smaller segment size than the log was written with.
			if err := d.rotate(); err != nil {
				return err
			}
			continue
		}
		n, seal := len(p), false
		if cur+int64(n) > d.segSize {
			from := max(0, d.segSize-cur)
			if i := bytes.IndexByte(p[from:], '\n'); i >= 0 {
				n, seal = int(from)+i+1, true
			} else if cur+int64(n) >= 2*d.segSize {
				n, seal = int(2*d.segSize-cur), true
			}
		}
		if err := d.write(p[:n]); err != nil {
			return err
		}
		p = p[n:]
		if seal {
			if err := d.rotate(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Dir[S]) write(p []byte) error {
	last := &d.segs[len(d.segs)-1]
	if _, err := d.active.WriteAt(p, last.size); err != nil {
		return fmt.Errorf("appending to log: %w", err)
	}
	lines := bytes.Count(p, []byte{'\n'})
	last.size += int64(len(p))
	last.lines += lines
	d.size += int64(len(p))
	d.lines += lines
	return nil
}

// openActive opens the last segment for writing, creating the first one for an empty log.
func (d *Dir[S]) openActive() error {
	if len(d.segs) == 0 {
		d.segs = append(d.segs, segment{})
	}
	// Owner-only: a session's output routinely contains whatever the user typed or read.
	f, err := os.OpenFile(d.segPath(d.segs[len(d.segs)-1].start), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("opening log segment: %w", err)
	}
	d.active = f
	return nil
}

// rotate seals the active segment, starts the next, and drops whatever retention no longer needs.
func (d *Dir[S]) rotate() error {
	if err := d.active.Close(); err != nil {
		return fmt.Errorf("sealing log segment: %w", err)
	}
	d.active = nil
	d.segs = append(d.segs, segment{start: d.end()})
	if err := d.openActive(); err != nil {
		return err
	}
	return d.retainLocked()
}

// retainLocked deletes the oldest sealed segments while what remains still covers the limits.
//
// Whole segments only, so the log keeps up to one segment more than a limit asks for. Rounding the
// other way would keep less than the user configured.
func (d *Dir[S]) retainLocked() error {
	for len(d.segs) > 1 {
		first := d.segs[0]
		byLines := d.limits.MaxLines > 0 && d.lines-first.lines >= d.limits.MaxLines
		byBytes := d.limits.MaxBytes > 0 && d.size-first.size >= d.limits.MaxBytes
		if !byLines && !byBytes {
			return nil
		}
		if err := os.Remove(d.segPath(first.start)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("dropping log segment: %w", err)
		}
		d.segs = d.segs[1:]
		d.size -= first.size
		d.lines -= first.lines
	}
	return nil
}

// Bounds returns the oldest retained sequence number and the next to be assigned.
func (d *Dir[S]) Bounds() (oldest, next S) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.segs) == 0 {
		return 0, 0
	}
	return S(d.segs[0].start), S(d.end())
}

// ReadFrom returns retained bytes starting at a sequence number, and whether bytes before it were
// already dropped.
func (d *Dir[S]) ReadFrom(from S) (data []byte, gap bool, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.segs) == 0 {
		return nil, false, nil
	}

	start := uint64(from)
	if start < d.segs[0].start {
		start, gap = d.segs[0].start, true
	}
	end := d.end()
	if start >= end {
		return nil, gap, nil
	}

	out := make([]byte, 0, end-start)
	for _, s := range d.segs {
		segEnd := s.start + uint64(s.size)
		if segEnd <= start {
			continue
		}
		off := int64(max(start, s.start) - s.start)
		buf := make([]byte, s.size-off)
		if err := readSegment(d.segPath(s.start), buf, off); err != nil {
			return nil, gap, err
		}
		out = append(out, buf...)
	}
	return out, gap, nil
}

func readSegment(path string, buf []byte, off int64) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("reading log: %w", err)
	}
	defer f.Close()
	if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("reading log: %w", err)
	}
	return nil
}

// Sync flushes the active segment to disk. Sealed segments were flushed by being closed.
func (d *Dir[S]) Sync() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	return d.active.Sync()
}

// Close flushes and releases the active segment.
func (d *Dir[S]) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.active == nil {
		return nil
	}
	err := d.active.Sync()
	if cerr := d.active.Close(); err == nil {
		err = cerr
	}
	d.active = nil
	return err
}

// RemoveDir deletes a segmented log: its segments, then the directory if nothing else is in it.
//
// Never RemoveAll. The path comes out of the database, and a wrong one there would otherwise delete
// a tree; unlinking only names that match the segment pattern bounds the damage to cm's own files.
func RemoveDir(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Type().IsRegular() && segmentPattern.MatchString(e.Name()) {
			if err := os.Remove(filepath.Join(path, e.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return os.Remove(path)
}

// MigrateFile moves a single-file log into a segmented log at dir, keeping its sequence numbering.
//
// For a session an older shim persisted, revived by a newer server. The new shim writes segments, and
// without moving the old content across the next reboot would restore only what followed the revival.
// The file is removed once the segment holding its content is written and synced.
func MigrateFile(file, dir string) error {
	src, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	if len(src) < fileHeaderBytes || !bytes.Equal(src[:len(fileMagic)], []byte(fileMagic)) {
		// Nothing File would have read either: it resets an unrecognized file.
		return os.Remove(file)
	}
	oldest := binary.BigEndian.Uint64(src[len(fileMagic):fileHeaderBytes])

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating log directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, segmentName(oldest)), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("creating log segment: %w", err)
	}
	_, err = f.Write(src[fileHeaderBytes:])
	if serr := f.Sync(); err == nil {
		err = serr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("writing log segment: %w", err)
	}
	return os.Remove(file)
}
