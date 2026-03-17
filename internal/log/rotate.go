package log

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RotationConfig controls built-in log rotation.
type RotationConfig struct {
	Period      time.Duration // how often to check (default 24h)
	Retention   time.Duration // keep lines newer than this (default 48h)
	MaxLineSize int           // scanner buffer size in bytes (default 64MB)
	ArchiveDir  string        // where to put .gz archives
	Files       []string      // absolute paths of log files to rotate
}

// RotateOnce performs a single rotation pass with the given config.
// Use Retention: 0 to archive all existing content (e.g. on startup).
func RotateOnce(cfg RotationConfig) {
	rotateAll(cfg)
}

// StartRotation starts a background goroutine that periodically rotates log files.
// It runs immediately on first call, then every cfg.Period thereafter.
// Returns a stop function that cancels the goroutine.
func StartRotation(cfg RotationConfig) func() {
	done := make(chan struct{})

	go func() {
		// Run immediately on startup.
		rotateAll(cfg)

		ticker := time.NewTicker(cfg.Period)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				rotateAll(cfg)
			}
		}
	}()

	return func() { close(done) }
}

func rotateAll(cfg RotationConfig) {
	for _, path := range cfg.Files {
		if err := rotateFile(path, cfg.Retention, cfg.ArchiveDir, cfg.MaxLineSize); err != nil {
			Warnf("rotate", "rotate %s: %v", path, err)
		}
	}
	// Reopen file handles so the logger writes to the new files.
	if err := Reopen(); err != nil {
		Errorf("rotate", "reopen after rotation: %v", err)
	}
}

// rotateFile processes a single log file: lines older than retention go to
// a gzip archive, recent lines stay in the active file.
func rotateFile(path string, retention time.Duration, archiveDir string, maxLineSize int) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // missing file is not an error
		}
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = f.Close() }()

	// Check if file is empty.
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat: %w", err)
	}
	if info.Size() == 0 {
		return nil
	}

	cutoff := time.Now().UTC().Add(-retention)

	// Fast path: if the first line is within retention, skip the file entirely.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxLineSize), maxLineSize) // 1MB line buffer
	if scanner.Scan() {
		ts, ok := parseTimestamp(path, scanner.Bytes())
		if ok && !ts.Before(cutoff) {
			return nil // entire file is recent
		}
	}

	// Rewind and stream through.
	if _, err := f.Seek(0, 0); err != nil {
		return fmt.Errorf("seek: %w", err)
	}

	// Ensure archive directory exists.
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		return fmt.Errorf("mkdir archive: %w", err)
	}

	// Create temp file for kept lines (same dir as source for atomic rename).
	dir := filepath.Dir(path)
	tmpFile, err := os.CreateTemp(dir, ".rotate-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer func() {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath) // clean up on error
	}()

	// Create temp archive file (renamed to final name after scanning determines timestamps).
	tmpArchive, err := os.CreateTemp(archiveDir, ".rotate-archive-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp archive: %w", err)
	}
	tmpArchivePath := tmpArchive.Name()
	defer func() {
		_ = tmpArchive.Close()
		_ = os.Remove(tmpArchivePath) // clean up on error
	}()
	gzw := gzip.NewWriter(tmpArchive)

	archivedLines := 0
	var archiveFirst, archiveLast time.Time
	seenRecent := false // have we encountered a line that's within retention?
	scanner = bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxLineSize), maxLineSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		ts, ok := parseTimestamp(path, line)
		if ok && ts.Before(cutoff) {
			// Old line → archive
			_, _ = gzw.Write(line)
			_, _ = gzw.Write([]byte("\n"))
			archivedLines++
			if archiveFirst.IsZero() || ts.Before(archiveFirst) {
				archiveFirst = ts
			}
			if ts.After(archiveLast) {
				archiveLast = ts
			}
		} else if !ok && !seenRecent {
			// Unparseable line before any recent content → archive with old lines.
			// These are typically non-log output (tool commands, diagnostics) that
			// predate the current retention window and would otherwise accumulate
			// permanently since they have no timestamp to age out.
			_, _ = gzw.Write(line)
			_, _ = gzw.Write([]byte("\n"))
			archivedLines++
		} else {
			// Recent timestamped line, or unparseable line after recent content → keep
			if ok {
				seenRecent = true
			}
			_, _ = tmpFile.Write(line)
			_, _ = tmpFile.Write([]byte("\n"))
		}
	}
	if err := scanner.Err(); err != nil {
		_ = gzw.Close()
		_ = os.Remove(tmpArchivePath)
		if err == bufio.ErrTooLong {
			Errorf("rotate", "%s: line exceeds rotation_max_line_size (%d bytes) — file will not be rotated; increase the limit in config", path, maxLineSize)
		}
		return fmt.Errorf("scan: %w", err)
	}

	// Finalize gzip.
	if err := gzw.Close(); err != nil {
		_ = os.Remove(tmpArchivePath)
		return fmt.Errorf("gzip close: %w", err)
	}
	_ = tmpArchive.Close()

	// If nothing was archived, remove the empty archive.
	if archivedLines == 0 {
		_ = os.Remove(tmpArchivePath)
		return nil
	}

	// If all archived lines were unparseable (no timestamps), use cutoff
	// as a reasonable fallback for the archive filename.
	if archiveFirst.IsZero() {
		archiveFirst = cutoff
		archiveLast = cutoff
	}

	// Replace the source file BEFORE committing the archive. If the source
	// rename fails, tmpArchivePath is still at its temp path and the defer
	// cleans it up — no orphaned archive, no data duplication. The reverse
	// order (archive first, then source) would leave a committed archive
	// with the source unchanged on failure, causing repeated re-archival of
	// the same lines.
	_ = tmpFile.Close()
	// os.CreateTemp creates files with 0600; match the 0644 used in log.go Init
	// so group/world read access isn't lost on rotation.
	_ = os.Chmod(tmpPath, 0640) // #nosec G302 - log file, group-readable for debugging
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}

	archivePath := archiveName(path, archiveDir, archiveFirst, archiveLast)
	if err := os.Rename(tmpArchivePath, archivePath); err != nil {
		return fmt.Errorf("rename archive: %w", err)
	}

	Infof("rotate", "rotated %s: archived %d old lines to %s", path, archivedLines, archivePath)
	return nil
}

// archiveName returns the archive path for a log file using the time range
// of archived content. This prevents overwrites on same-day restarts.
//
// e.g. foci.log → archive/foci-2026-03-01T17:00:00Z--2026-03-01T19:15:00Z.log.gz
func archiveName(path, archiveDir string, first, last time.Time) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)              // .jsonl or .log
	name := strings.TrimSuffix(base, ext)  // api-payload or foci

	startStr := first.UTC().Format("2006-01-02T15:04:05Z")
	endStr := last.UTC().Format("2006-01-02T15:04:05Z")

	return filepath.Join(archiveDir, fmt.Sprintf("%s-%s--%s%s.gz", name, startStr, endStr, ext))
}

// parseTimestamp dispatches to the right parser based on file extension.
func parseTimestamp(path string, line []byte) (time.Time, bool) {
	if strings.HasSuffix(path, ".jsonl") {
		return parseJSONLTimestamp(line)
	}
	return parseEventTimestamp(line)
}

// parseJSONLTimestamp extracts the "ts" field from a JSONL line by byte scanning.
// Avoids full JSON unmarshal for efficiency on multi-GB files.
func parseJSONLTimestamp(line []byte) (time.Time, bool) {
	// Look for "ts":"..." pattern
	key := []byte(`"ts":"`)
	idx := bytes.Index(line, key)
	if idx < 0 {
		return time.Time{}, false
	}
	start := idx + len(key)
	end := bytes.IndexByte(line[start:], '"')
	if end < 0 {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339Nano, string(line[start:start+end]))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}

// parseEventTimestamp extracts an RFC3339 timestamp from the start of a foci.log line.
// Log format: "2026-02-25T12:34:56Z INFO  [component] message"
func parseEventTimestamp(line []byte) (time.Time, bool) {
	if len(line) == 0 {
		return time.Time{}, false
	}
	// Find first space — everything before it is the timestamp.
	spaceIdx := bytes.IndexByte(line, ' ')
	if spaceIdx < 0 {
		return time.Time{}, false
	}
	ts, err := time.Parse(time.RFC3339, string(line[:spaceIdx]))
	if err != nil {
		return time.Time{}, false
	}
	return ts, true
}
