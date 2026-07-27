// Package tail reads the end of an append-only JSONL log without re-reading
// what it read last time.
//
// Agent session logs run to tens of megabytes and are polled once a second
// while a preview is open. Reading the tail from scratch on every poll costs
// hundreds of milliseconds and tens of megabytes of garbage per second, which
// is felt in the UI as scroll stutter rather than as slow previews.
package tail

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"
)

// Scanner folds log lines into whatever state its caller needs.
type Scanner interface {
	// Consume folds every complete line in buffer into the scanner and returns
	// the offset just past the last newline it read. Anything after that offset
	// is a record still being written, and is offered again on the next read.
	// startsMidLine reports that the buffer opens inside a record whose leading
	// fragment must be discarded.
	Consume(buffer []byte, startsMidLine bool) int64

	// Complete reports whether the scanner has all it needs, which stops a cold
	// read from widening its window any further.
	Complete() bool
}

// Lines calls visit for every complete line in buffer and returns the offset
// just past the last newline.
func Lines(buffer []byte, startsMidLine bool, visit func(line []byte)) int64 {
	offset := 0
	if startsMidLine {
		end := bytes.IndexByte(buffer, '\n')
		if end < 0 {
			return 0
		}
		offset = end + 1
	}
	for {
		end := bytes.IndexByte(buffer[offset:], '\n')
		if end < 0 {
			return int64(offset)
		}
		visit(buffer[offset : offset+end])
		offset += end + 1
	}
}

const (
	// DefaultInitialWindow is how much of a log's end a cold read looks at
	// before it starts doubling.
	DefaultInitialWindow = 512 << 10
	// MaxWindow caps a cold read. Past this, an answer built from the visible
	// tail beats stalling the caller on a hundred-megabyte history.
	MaxWindow = 16 << 20
)

// Reader keeps one scanner up to date with the end of one file. Its zero value
// is ready to use. It is safe for concurrent use, though a second file simply
// displaces the first: previews look at one session at a time, so a single slot
// is all that is needed and it keeps the retained state bounded.
type Reader struct {
	mu sync.Mutex

	scanner Scanner
	path    string
	limit   int
	size    int64
	offset  int64
	modTime time.Time
}

// Scan returns a scanner covering the end of path. The scanner from the
// previous call is extended with whatever was appended since; it is rebuilt
// with newScanner only when it cannot be extended — a different file, a
// different limit, or a log that was replaced rather than appended to.
func (r *Reader) Scan(
	ctx context.Context,
	path string,
	limit int,
	newScanner func() Scanner,
) (Scanner, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	size := info.Size()

	if r.scanner != nil && r.path == path && r.limit == limit && size >= r.size {
		if size == r.size && info.ModTime().Equal(r.modTime) {
			return r.scanner, nil
		}
		if err := r.extend(ctx, file, size, info.ModTime()); err != nil {
			r.forget()
			return nil, err
		}
		return r.scanner, nil
	}

	scanner, offset, err := readTail(
		ctx, file, size, DefaultInitialWindow, MaxWindow, newScanner,
	)
	if err != nil {
		r.forget()
		return nil, err
	}
	r.scanner = scanner
	r.path = path
	r.limit = limit
	r.size = size
	r.offset = offset
	r.modTime = info.ModTime()
	return scanner, nil
}

func (r *Reader) forget() {
	r.scanner = nil
	r.path = ""
}

// extend folds in only the bytes appended since the previous read.
func (r *Reader) extend(
	ctx context.Context,
	file *os.File,
	size int64,
	modTime time.Time,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	buffer := make([]byte, size-r.offset)
	n, err := file.ReadAt(buffer, r.offset)
	if err != nil && err != io.EOF {
		return fmt.Errorf("read log: %w", err)
	}
	r.offset += r.scanner.Consume(buffer[:n], false)
	r.size = size
	r.modTime = modTime
	return nil
}

// ReadTail fills a scanner from the end of path in one shot, keeping no state
// between calls. It suits callers that sweep many files once, where a cache
// would only thrash.
func ReadTail(
	ctx context.Context,
	path string,
	initialWindow, maxWindow int64,
	newScanner func() Scanner,
) (Scanner, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open log: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	scanner, _, err := readTail(
		ctx, file, info.Size(), initialWindow, maxWindow, newScanner,
	)
	return scanner, err
}

// readTail widens its window until the scanner is satisfied, and reports the
// offset just past the last line it consumed.
func readTail(
	ctx context.Context,
	file *os.File,
	size, initialWindow, maxWindow int64,
	newScanner func() Scanner,
) (Scanner, int64, error) {
	window := initialWindow
	for {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		window = min(window, size)
		start := size - window
		buffer := make([]byte, window)
		n, err := file.ReadAt(buffer, start)
		if err != nil && err != io.EOF {
			return nil, 0, fmt.Errorf("read log: %w", err)
		}
		scanner := newScanner()
		consumed := scanner.Consume(buffer[:n], start > 0)
		if scanner.Complete() || window >= size || window >= maxWindow {
			return scanner, start + consumed, nil
		}
		window = min(window*2, maxWindow)
	}
}
