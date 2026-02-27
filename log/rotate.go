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
	defer f.Close()

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
		tmpFile.Close()
		os.Remove(tmpPath) // clean up on error
	}()

	// Create archive file.
	archivePath := archiveName(path, archiveDir)
	archiveFile, err := os.Create(archivePath)
	if err != nil {
		return fmt.Errorf("create archive %s: %w", archivePath, err)
	}
	gzw := gzip.NewWriter(archiveFile)

	archivedLines := 0
	scanner = bufio.NewScanner(f)
	scanner.Buffer(make([]byte, maxLineSize), maxLineSize)

	for scanner.Scan() {
		line := scanner.Bytes()
		ts, ok := parseTimestamp(path, line)
		if ok && ts.Before(cutoff) {
			// Old line → archive
			gzw.Write(line)
			gzw.Write([]byte("\n"))
			archivedLines++
		} else {
			// Recent or unparseable → keep
			tmpFile.Write(line)
			tmpFile.Write([]byte("\n"))
		}
	}
	if err := scanner.Err(); err != nil {
		gzw.Close()
		archiveFile.Close()
		os.Remove(archivePath)
		return fmt.Errorf("scan: %w", err)
	}

	// Finalize gzip.
	if err := gzw.Close(); err != nil {
		archiveFile.Close()
		os.Remove(archivePath)
		return fmt.Errorf("gzip close: %w", err)
	}
	archiveFile.Close()

	// If nothing was archived, remove the empty archive.
	if archivedLines == 0 {
		os.Remove(archivePath)
		return nil
	}

	// Close temp file and atomically replace the original.
	tmpFile.Close()
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("rename temp to %s: %w", path, err)
	}

	Infof("rotate", "rotated %s: archived %d old lines to %s", path, archivedLines, archivePath)
	return nil
}

// archiveName returns the archive path for a log file.
// e.g. api-payload.jsonl → archive/api-payload-2026-02-25.jsonl.gz
//
//	foci.log       → archive/foci-2026-02-25.log.gz
func archiveName(path, archiveDir string) string {
	base := filepath.Base(path)
	date := time.Now().UTC().Format("2006-01-02")

	ext := filepath.Ext(base)            // .jsonl or .log
	name := strings.TrimSuffix(base, ext) // api-payload or foci

	return filepath.Join(archiveDir, fmt.Sprintf("%s-%s%s.gz", name, date, ext))
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
