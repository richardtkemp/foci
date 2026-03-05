package tools

import (
	"bytes"
	"io"
	"os"
	"sync"
)

// spillWriter keeps the first threshold bytes in memory and overflows
// to a temp file. This avoids buffering 100MB of shell output in RAM
// when the guard will immediately write it to disk anyway.
type spillWriter struct {
	mu        sync.Mutex
	head      bytes.Buffer
	file      *os.File
	total     int64
	threshold int64
	spilled   bool
	tempDir   string
}

// newSpillWriter creates a spillWriter that keeps threshold bytes in memory.
// tempDir is the directory for overflow temp files (empty = os default).
func newSpillWriter(threshold int64, tempDir string) *spillWriter {
	return &spillWriter{
		threshold: threshold,
		tempDir:   tempDir,
	}
}

// Write implements io.Writer. Thread-safe for concurrent stdout/stderr writes.
func (w *spillWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.total += int64(len(p))

	if w.spilled {
		return w.file.Write(p)
	}

	// Check if this write would overflow the threshold
	if int64(w.head.Len())+int64(len(p)) <= w.threshold {
		return w.head.Write(p)
	}

	// Spill: create temp file, dump head + new data
	var err error
	w.file, err = os.CreateTemp(w.tempDir, "foci-spill-*.tmp")
	if err != nil {
		// Can't create temp file — keep writing to head (best effort)
		return w.head.Write(p)
	}
	w.spilled = true

	// Write existing head contents to file
	if w.head.Len() > 0 {
		if _, err := w.file.Write(w.head.Bytes()); err != nil {
			return 0, err
		}
		w.head.Reset()
	}

	return w.file.Write(p)
}

// Close closes the temp file if one was created.
func (w *spillWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Close()
	}
	return nil
}

// Cleanup closes and removes the temp file if one was created.
func (w *spillWriter) Cleanup() {
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
func (w *spillWriter) FilePath() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file != nil {
		return w.file.Name()
	}
	return ""
}

// Spilled returns true if overflow occurred.
func (w *spillWriter) Spilled() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.spilled
}

// Total returns the total bytes written.
func (w *spillWriter) Total() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.total
}

// String returns the head portion: the in-memory buffer if not spilled,
// or the first threshold bytes read back from the temp file if spilled.
func (w *spillWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()

	if !w.spilled {
		return w.head.String()
	}

	// Read the first threshold bytes from the temp file
	buf := make([]byte, w.threshold)
	n, err := w.file.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		return ""
	}
	return string(buf[:n])
}
