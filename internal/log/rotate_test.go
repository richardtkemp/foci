package log

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseJSONLTimestamp(t *testing.T) {
	// Verifies that parseJSONLTimestamp correctly extracts
	// RFC3339 timestamps from JSONL lines, handling nanoseconds, missing fields,
	// malformed values, and empty input.
	tests := []struct {
		name   string
		line   string
		wantOK bool
		wantTS string
	}{
		{
			name:   "valid",
			line:   `{"ts":"2026-02-20T10:00:00Z","model":"claude","input":100}`,
			wantOK: true,
			wantTS: "2026-02-20T10:00:00Z",
		},
		{
			name:   "valid with nanos",
			line:   `{"ts":"2026-02-20T10:00:00.123456Z","session":"main"}`,
			wantOK: true,
			wantTS: "2026-02-20T10:00:00.123456Z",
		},
		{
			name:   "missing ts field",
			line:   `{"model":"claude","input":100}`,
			wantOK: false,
		},
		{
			name:   "malformed ts",
			line:   `{"ts":"not-a-date","model":"claude"}`,
			wantOK: false,
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, ok := parseJSONLTimestamp([]byte(tt.line))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				want, _ := time.Parse(time.RFC3339Nano, tt.wantTS)
				if !ts.Equal(want) {
					t.Fatalf("ts = %v, want %v", ts, want)
				}
			}
		})
	}
}

func TestParseEventTimestamp(t *testing.T) {
	// Verifies that parseEventTimestamp extracts RFC3339
	// timestamps from the first token of a plain-text event log line, returning
	// false for empty lines, missing space separators, and invalid date strings.
	tests := []struct {
		name   string
		line   string
		wantOK bool
		wantTS string
	}{
		{
			name:   "valid",
			line:   "2026-02-20T10:00:00Z INFO  [main] started",
			wantOK: true,
			wantTS: "2026-02-20T10:00:00Z",
		},
		{
			name:   "empty line",
			line:   "",
			wantOK: false,
		},
		{
			name:   "no space",
			line:   "notimestamp",
			wantOK: false,
		},
		{
			name:   "bad timestamp",
			line:   "not-a-date INFO  [main] started",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, ok := parseEventTimestamp([]byte(tt.line))
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok {
				want, _ := time.Parse(time.RFC3339, tt.wantTS)
				if !ts.Equal(want) {
					t.Fatalf("ts = %v, want %v", ts, want)
				}
			}
		})
	}
}

func TestRotateFile(t *testing.T) {
	// Verifies the core rotation logic: old lines are moved to a
	// gzip archive, recent lines stay in the active file, and corrupt (unparseable)
	// lines are retained in the active file rather than dropped.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)
	corrupt := "this line has no timestamp"

	lines := []string{
		`{"ts":"` + old + `","msg":"old1"}`,
		`{"ts":"` + old + `","msg":"old2"}`,
		corrupt,
		`{"ts":"` + recent + `","msg":"new1"}`,
		`{"ts":"` + recent + `","msg":"new2"}`,
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile: %v", err)
	}

	// Check active file: should have corrupt line + 2 recent lines.
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read active: %v", err)
	}
	kept := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(kept) != 3 {
		t.Fatalf("kept %d lines, want 3: %v", len(kept), kept)
	}
	if kept[0] != corrupt {
		t.Errorf("kept[0] = %q, want corrupt line", kept[0])
	}

	// Check archive exists and contains 2 old lines.
	archives, _ := filepath.Glob(filepath.Join(archiveDir, "*.jsonl.gz"))
	if len(archives) != 1 {
		t.Fatalf("expected 1 archive, got %d", len(archives))
	}
	archived := readGzip(t, archives[0])
	archivedLines := strings.Split(strings.TrimSpace(archived), "\n")
	if len(archivedLines) != 2 {
		t.Fatalf("archived %d lines, want 2", len(archivedLines))
	}
}

func TestRotateFileAllFresh(t *testing.T) {
	// Verifies that rotateFile is a no-op when all lines are
	// within the retention window: the file is unchanged and no archive is created.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	recent := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	lines := `{"ts":"` + recent + `","msg":"new1"}` + "\n" +
		`{"ts":"` + recent + `","msg":"new2"}` + "\n"
	os.WriteFile(logPath, []byte(lines), 0644)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile: %v", err)
	}

	// File should be unchanged.
	data, _ := os.ReadFile(logPath)
	if string(data) != lines {
		t.Errorf("file was modified when all lines are fresh")
	}

	// No archive should be created.
	archives, _ := filepath.Glob(filepath.Join(archiveDir, "*.gz"))
	if len(archives) != 0 {
		t.Errorf("unexpected archives: %v", archives)
	}
}

func TestRotateFileEmpty(t *testing.T) {
	// Verifies that rotateFile handles an empty log file without
	// error, producing no archive.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(logPath, []byte{}, 0644)

	err := rotateFile(logPath, 48*time.Hour, filepath.Join(dir, "archive"), 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile on empty: %v", err)
	}
}

func TestRotateFileMissing(t *testing.T) {
	// Verifies that rotateFile treats a non-existent file as a
	// no-op, returning nil rather than an error.
	err := rotateFile("/nonexistent/path/log.jsonl", 48*time.Hour, "/tmp/archive", 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile on missing: %v", err)
	}
}

func TestRotateFileArchiveNaming(t *testing.T) {
	// Verifies that archiveName produces correctly formatted
	// archive filenames for different log file extensions (.jsonl and .log), embedding
	// first-line and last-line timestamps in the name.
	first := time.Date(2026, 3, 1, 17, 0, 0, 0, time.UTC)
	last := time.Date(2026, 3, 1, 19, 15, 0, 0, time.UTC)

	tests := []struct {
		path string
		want string
	}{
		{"api-payload.jsonl", "api-payload-2026-03-01T17:00:00Z--2026-03-01T19:15:00Z.jsonl.gz"},
		{"foci.log", "foci-2026-03-01T17:00:00Z--2026-03-01T19:15:00Z.log.gz"},
		{"api.jsonl", "api-2026-03-01T17:00:00Z--2026-03-01T19:15:00Z.jsonl.gz"},
	}

	for _, tt := range tests {
		got := filepath.Base(archiveName(tt.path, "/archive", first, last))
		if got != tt.want {
			t.Errorf("archiveName(%q) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestRotateFileArchiveNamingSpansDays(t *testing.T) {
	// Verifies that archiveName correctly handles
	// a time range that crosses a day boundary (e.g. Feb 28 into Mar 1).
	first := time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC)
	last := time.Date(2026, 3, 1, 1, 30, 0, 0, time.UTC)

	got := filepath.Base(archiveName("foci.log", "/archive", first, last))
	want := "foci-2026-02-28T23:00:00Z--2026-03-01T01:30:00Z.log.gz"
	if got != want {
		t.Errorf("archiveName = %q, want %q", got, want)
	}
}

func TestRotateFileEventLog(t *testing.T) {
	// Verifies that rotation works on plain-text event log
	// files (not JSONL), using the space-separated timestamp parser to split old
	// from recent lines.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "foci.log")

	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	lines := []string{
		old + " INFO  [main] old message",
		recent + " WARN  [main] new message",
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0644)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile: %v", err)
	}

	// Active should have 1 line.
	data, _ := os.ReadFile(logPath)
	kept := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(kept) != 1 {
		t.Fatalf("kept %d lines, want 1", len(kept))
	}

	// Archive should have 1 line.
	archives, _ := filepath.Glob(filepath.Join(archiveDir, "*.log.gz"))
	if len(archives) != 1 {
		t.Fatalf("expected 1 archive, got %d", len(archives))
	}
}

func TestRotateFile_PreservesPermissions(t *testing.T) {
	// Verifies that after rotation the active log file has 0640 permissions,
	// not 0600 (the default from os.CreateTemp). This ensures group-read
	// access survives rotation.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "foci.log")

	now := time.Now().UTC()
	old := now.Add(-72 * time.Hour).Format(time.RFC3339)
	recent := now.Add(-1 * time.Hour).Format(time.RFC3339)

	lines := []string{
		old + " INFO  [main] old message",
		recent + " WARN  [main] new message",
	}
	os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0640)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile: %v", err)
	}

	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat after rotation: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0640 {
		t.Errorf("permissions after rotation = %o, want 0640", perm)
	}
}

func TestStartRotationStop(t *testing.T) {
	// Verifies that StartRotation launches a background goroutine
	// that can be cleanly stopped via the returned stop function within a reasonable timeout.
	stop := StartRotation(RotationConfig{
		Period:      100 * time.Millisecond,
		Retention:   48 * time.Hour,
		MaxLineSize: 1024 * 1024,
		ArchiveDir:  t.TempDir(),
		Files:       nil, // no files to rotate
	})

	// Let it tick at least once.
	time.Sleep(150 * time.Millisecond)

	// Stop should return without blocking.
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()

	select {
	case <-done:
		// ok
	case <-time.After(2 * time.Second):
		t.Fatal("stop() did not return within 2s")
	}
}

func TestRotateFileAllOld(t *testing.T) {
	// Verifies that when all lines are old, the active file
	// is left empty and everything goes to the archive.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	lines := `{"ts":"` + old + `","msg":"old1"}` + "\n" +
		`{"ts":"` + old + `","msg":"old2"}` + "\n"
	os.WriteFile(logPath, []byte(lines), 0644)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile: %v", err)
	}

	// Active file should be empty
	data, _ := os.ReadFile(logPath)
	if strings.TrimSpace(string(data)) != "" {
		t.Errorf("active file should be empty, got %q", string(data))
	}

	// Archive should contain both lines
	archives, _ := filepath.Glob(filepath.Join(archiveDir, "*.jsonl.gz"))
	if len(archives) != 1 {
		t.Fatalf("expected 1 archive, got %d", len(archives))
	}
	archived := readGzip(t, archives[0])
	archivedLines := strings.Split(strings.TrimSpace(archived), "\n")
	if len(archivedLines) != 2 {
		t.Fatalf("archived %d lines, want 2", len(archivedLines))
	}
}

func TestRotateAllWithFailingFile(t *testing.T) {
	// Verifies rotateAll logs a warning when a file fails to rotate.
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	setOutput(&buf)
	setLevel(DEBUG)

	dir := t.TempDir()
	// Use a path inside a non-writable directory to trigger mkdir failure for archive
	badPath := filepath.Join(dir, "test.jsonl")
	os.WriteFile(badPath, []byte(`{"ts":"`+time.Now().UTC().Add(-72*time.Hour).Format(time.RFC3339)+`","msg":"old"}`+"\n"), 0644)

	rotateAll(RotationConfig{
		Period:      time.Hour,
		Retention:   48 * time.Hour,
		MaxLineSize: 1024 * 1024,
		ArchiveDir:  "/nonexistent/archive/dir",
		Files:       []string{badPath},
	})

	output := buf.String()
	if !strings.Contains(output, "WARN") || !strings.Contains(output, "rotate") {
		t.Errorf("expected warning about rotate failure, got: %s", output)
	}
}

func TestRotateFileLineTooLong(t *testing.T) {
	// Verifies that rotateFile handles lines exceeding max buffer size.
	resetGlobal()
	defer resetGlobal()

	var buf bytes.Buffer
	setOutput(&buf)

	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	// Create a line that exceeds the max line size
	longLine := `{"ts":"` + old + `","msg":"` + strings.Repeat("x", 200) + `"}`
	os.WriteFile(logPath, []byte(longLine+"\n"), 0644)

	// Use a very small max line size to trigger ErrTooLong
	err := rotateFile(logPath, 48*time.Hour, archiveDir, 32)
	if err == nil {
		t.Fatal("expected error for too-long line")
	}
	if !strings.Contains(err.Error(), "scan") {
		t.Errorf("expected scan error, got: %v", err)
	}
	// Should log an error about the line being too long
	if !strings.Contains(buf.String(), "rotation_max_line_size") {
		t.Errorf("expected rotation_max_line_size error, got: %s", buf.String())
	}
}

func TestParseJSONLTimestampUnterminatedQuote(t *testing.T) {
	// Verifies that a JSONL line with
	// "ts":" but no closing quote returns false.
	line := `{"ts":"2026-02-20T10:00:00Z`
	_, ok := parseJSONLTimestamp([]byte(line))
	if ok {
		t.Error("expected false for unterminated quote")
	}
}

func TestParseTimestampDispatch(t *testing.T) {
	// JSONL path
	ts, ok := parseTimestamp("api.jsonl", []byte(`{"ts":"2026-02-20T10:00:00Z","data":1}`))
	if !ok {
		t.Fatal("expected ok for JSONL")
	}
	want, _ := time.Parse(time.RFC3339, "2026-02-20T10:00:00Z")
	if !ts.Equal(want) {
		t.Errorf("JSONL ts = %v, want %v", ts, want)
	}

	// Event log path
	ts, ok = parseTimestamp("foci.log", []byte("2026-02-20T10:00:00Z INFO  [main] hello"))
	if !ok {
		t.Fatal("expected ok for event log")
	}
	if !ts.Equal(want) {
		t.Errorf("event ts = %v, want %v", ts, want)
	}
}

func TestStartRotationRunsImmediately(t *testing.T) {
	// Verifies the rotation goroutine runs immediately on start
	// and processes files in the config.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	os.WriteFile(logPath, []byte(`{"ts":"`+old+`","msg":"old"}`+"\n"), 0644)

	stop := StartRotation(RotationConfig{
		Period:      1 * time.Hour, // long period — we rely on immediate run
		Retention:   48 * time.Hour,
		MaxLineSize: 1024 * 1024,
		ArchiveDir:  archiveDir,
		Files:       []string{logPath},
	})
	defer stop()

	// Wait for the immediate run to complete
	time.Sleep(200 * time.Millisecond)

	// Archive should exist from the immediate run
	archives, _ := filepath.Glob(filepath.Join(archiveDir, "*.jsonl.gz"))
	if len(archives) != 1 {
		t.Errorf("expected 1 archive from immediate run, got %d", len(archives))
	}
}

func TestRotateFileAllUnparseable(t *testing.T) {
	// Verifies that when all lines have no parseable
	// timestamp, nothing is archived and the active file is unchanged.
	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	content := "no timestamp here\nalso no timestamp\n"
	os.WriteFile(logPath, []byte(content), 0644)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile: %v", err)
	}

	// File should be unchanged — unparseable lines are kept
	data, _ := os.ReadFile(logPath)
	if string(data) != content {
		t.Errorf("file was modified when all lines are unparseable")
	}
}

func TestRotateAllReopenError(t *testing.T) {
	// Verifies that rotateAll logs an error when
	// Reopen fails after rotation.
	resetGlobal()
	defer resetGlobal()

	dir := t.TempDir()
	archiveDir := filepath.Join(dir, "archive")
	logPath := filepath.Join(dir, "test.jsonl")

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	os.WriteFile(logPath, []byte(`{"ts":"`+old+`","msg":"old"}`+"\n"), 0644)

	// Set up the logger with a valid event file, then break its path
	eventPath := filepath.Join(dir, "foci.log")
	err := Init(Config{Level: "INFO", EventFile: eventPath})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	// Capture output after Init (Init replaces eventOut)
	var buf bytes.Buffer
	setOutput(&buf)

	// Break the event path so Reopen fails
	std.mu.Lock()
	std.eventPath = "/nonexistent/dir/foci.log"
	std.mu.Unlock()

	rotateAll(RotationConfig{
		Period:      time.Hour,
		Retention:   48 * time.Hour,
		MaxLineSize: 1024 * 1024,
		ArchiveDir:  archiveDir,
		Files:       []string{logPath},
	})

	if !strings.Contains(buf.String(), "reopen after rotation") {
		t.Errorf("expected reopen error log, got: %s", buf.String())
	}
}

// Note: rotateFile stat/seek errors are defensive code for OS-level failures
// that can't be reliably triggered in unit tests.

func TestRotateFileCreateTempError(t *testing.T) {
	// Verifies rotateFile handles temp file creation errors.
	srcDir := t.TempDir()
	logPath := filepath.Join(srcDir, "test.jsonl")

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	os.WriteFile(logPath, []byte(`{"ts":"`+old+`","msg":"old"}`+"\n"), 0644)

	// Make the log dir read-only so CreateTemp fails
	os.Chmod(srcDir, 0555)
	defer os.Chmod(srcDir, 0755)

	archiveDir := t.TempDir()
	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err == nil {
		t.Fatal("expected error when source dir is read-only")
	}
	if !strings.Contains(err.Error(), "create temp") {
		t.Errorf("expected 'create temp' error, got: %v", err)
	}
}

func TestRotateFileCreateTempArchiveError(t *testing.T) {
	// Verifies rotateFile handles archive temp file creation errors.
	srcDir := t.TempDir()
	logPath := filepath.Join(srcDir, "test.jsonl")

	old := time.Now().UTC().Add(-72 * time.Hour).Format(time.RFC3339)
	os.WriteFile(logPath, []byte(`{"ts":"`+old+`","msg":"old"}`+"\n"), 0644)

	// Archive dir exists but is read-only
	archiveDir := filepath.Join(t.TempDir(), "archive")
	os.MkdirAll(archiveDir, 0555)
	defer os.Chmod(archiveDir, 0755)

	err := rotateFile(logPath, 48*time.Hour, archiveDir, 1024*1024)
	if err == nil {
		t.Fatal("expected error when archive dir is read-only")
	}
	if !strings.Contains(err.Error(), "create temp archive") {
		t.Errorf("expected 'create temp archive' error, got: %v", err)
	}
}

func TestRotateFileOpenError(t *testing.T) {
	// Verifies rotateFile returns an error for a non-NotExist open failure.
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.jsonl")

	// Create the file then make it unreadable
	os.WriteFile(logPath, []byte("data\n"), 0644)
	os.Chmod(logPath, 0000)
	defer os.Chmod(logPath, 0644)

	err := rotateFile(logPath, 48*time.Hour, filepath.Join(dir, "archive"), 1024*1024)
	if err == nil {
		t.Fatal("expected error when file is unreadable")
	}
	if !strings.Contains(err.Error(), "open") {
		t.Errorf("expected 'open' error, got: %v", err)
	}
}

// Note: rotateFile rename errors (source rename, archive rename) are OS-level
// defensive paths that require blocking rename while allowing temp file creation,
// which is not reliably testable without OS-level tricks.

func readGzip(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open gzip: %v", err)
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	defer r.Close()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read gzip: %v", err)
	}
	return string(data)
}
