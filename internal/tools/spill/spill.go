// Package spill provides a writer that keeps a bounded prefix of a stream in
// memory and overflows the remainder to a temp file. It lets tools capture
// large, unknown-length output (shell command stdout, HTTP response bodies)
// without buffering the whole thing in RAM: the in-memory head is the model-
// facing preview, and the full output lands on disk for later reading.
package spill

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// Writer keeps the first threshold bytes in memory and overflows to a temp
// file. An optional maxTotal caps the total bytes retained (0 = unbounded):
// writes past the cap are dropped and Truncated reports true. This bounds disk
// use when the source is untrusted/remote (HTTP), while local sources (shell)
// leave maxTotal at 0.
type Writer struct {
	mu        sync.Mutex
	head      bytes.Buffer
	file      *os.File
	total     int64
	written   int64 // bytes actually retained (<= maxTotal when capped)
	threshold int64
	maxTotal  int64
	spilled   bool
	truncated bool
	tempDir   string
}

// New creates a Writer that keeps threshold bytes in memory before overflowing
// to a temp file in tempDir (empty = os default). maxTotal caps retained bytes
// (0 = unbounded).
func New(threshold, maxTotal int64, tempDir string) *Writer {
	return &Writer{
		threshold: threshold,
		maxTotal:  maxTotal,
		tempDir:   tempDir,
	}
}

// Write implements io.Writer. Thread-safe for concurrent stdout/stderr writes.
// total always reflects the source length seen; written reflects bytes retained
// after any maxTotal cap.
func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	n := len(p)
	w.total += int64(n)

	// Apply the cap: keep only up to maxTotal bytes; report the full write so
	// the caller's io.Copy doesn't error, but drop the overflow.
	if w.maxTotal > 0 {
		room := w.maxTotal - w.written
		if room <= 0 {
			w.truncated = true
			return n, nil
		}
		if int64(len(p)) > room {
			p = p[:room]
			w.truncated = true
		}
	}
	w.written += int64(len(p))

	if w.spilled {
		if _, err := w.file.Write(p); err != nil {
			return 0, err
		}
		return n, nil
	}

	// Within the in-memory threshold: keep in head.
	if int64(w.head.Len())+int64(len(p)) <= w.threshold {
		if _, err := w.head.Write(p); err != nil {
			return 0, err
		}
		return n, nil
	}

	// Spill: create temp file, dump head + new data.
	var err error
	w.file, err = os.CreateTemp(w.tempDir, "foci-spill-*.tmp")
	if err != nil {
		// Can't create temp file — keep writing to head (best effort).
		if _, werr := w.head.Write(p); werr != nil {
			return 0, werr
		}
		return n, nil
	}
	w.spilled = true

	if w.head.Len() > 0 {
		if _, err := w.file.Write(w.head.Bytes()); err != nil {
			return 0, err
		}
		w.head.Reset()
	}
	if _, err := w.file.Write(p); err != nil {
		return 0, err
	}
	return n, nil
}

// Close closes the temp file if one was created.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// Cleanup closes and removes the temp file if one was created.
func (w *Writer) Cleanup() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		name := w.file.Name()
		_ = w.file.Close()
		_ = os.Remove(name)
		w.file = nil
	}
}

// FilePath returns the temp file path if spilled, else empty string.
func (w *Writer) FilePath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Name()
	}
	return ""
}

// Spilled returns true if overflow to a temp file occurred.
func (w *Writer) Spilled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.spilled
}

// Truncated returns true if the maxTotal cap dropped any bytes.
func (w *Writer) Truncated() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.truncated
}

// Total returns the total bytes seen from the source (before any cap).
func (w *Writer) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// String returns the head portion: the in-memory buffer if not spilled, or the
// first threshold bytes read back from the temp file if spilled.
func (w *Writer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.spilled {
		return w.head.String()
	}

	buf := make([]byte, w.threshold)
	n, err := w.file.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return ""
	}
	return string(buf[:n])
}
