package log

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseJSONLTimestamp(t *testing.T) {
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
	dir := t.TempDir()
	logPath := filepath.Join(dir, "empty.jsonl")
	os.WriteFile(logPath, []byte{}, 0644)

	err := rotateFile(logPath, 48*time.Hour, filepath.Join(dir, "archive"), 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile on empty: %v", err)
	}
}

func TestRotateFileMissing(t *testing.T) {
	err := rotateFile("/nonexistent/path/log.jsonl", 48*time.Hour, "/tmp/archive", 1024*1024)
	if err != nil {
		t.Fatalf("rotateFile on missing: %v", err)
	}
}

func TestRotateFileArchiveNaming(t *testing.T) {
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
	first := time.Date(2026, 2, 28, 23, 0, 0, 0, time.UTC)
	last := time.Date(2026, 3, 1, 1, 30, 0, 0, time.UTC)

	got := filepath.Base(archiveName("foci.log", "/archive", first, last))
	want := "foci-2026-02-28T23:00:00Z--2026-03-01T01:30:00Z.log.gz"
	if got != want {
		t.Errorf("archiveName = %q, want %q", got, want)
	}
}

func TestRotateFileEventLog(t *testing.T) {
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

func TestStartRotationStop(t *testing.T) {
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
