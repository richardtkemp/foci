package main

import (
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/timeutil"
)

// TestInitLogging_StartupRotationKeepsThisLifetimesEarlyLines reproduces
// foci_todo #1869: main's early log.Init writes to the event file before
// config load (the auto_approve permissions report, shellenv, preload), and
// initLogging's startup rotation then archived EVERYTHING up to "now" — so the
// live foci.log began at the [rotate] line and this lifetime's opening WARNs
// existed only in the archive. The startup pass must archive the previous
// lifetime and nothing this process wrote.
func TestInitLogging_StartupRotationKeepsThisLifetimesEarlyLines(t *testing.T) {
	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	archiveDir := filepath.Join(dir, "archive")

	processStart := time.Now().Truncate(time.Second)

	// The outgoing process's final line. Stamped minutes (not days) back so
	// StartRotation's immediate 48h-retention pass cannot be what archives it.
	const outgoing = "outgoing-process-final-line"
	prev := timeutil.Format(processStart.Add(-10*time.Minute)) + " INFO  [main] " + outgoing + "\n"
	if err := os.WriteFile(eventPath, []byte(prev), 0600); err != nil {
		t.Fatal(err)
	}

	// main's early init, then a deliberate early WARN like config.Load's.
	if err := log.Init(log.Config{EventFile: eventPath}); err != nil {
		t.Fatal(err)
	}
	const early = "early-boot-warn-1869"
	log.Warnf("config", "%s", early)

	cfg := &config.Config{Logging: config.LoggingConfig{
		Level:               "INFO",
		EventFile:           eventPath,
		ArchiveDir:          archiveDir,
		LogRotation:         config.Ptr(true),
		RotationPeriod:      "24h",
		RetentionPeriod:     "48h",
		RotationMaxLineSize: "1MB",
		LogFileMode:         "0600",
		ConversationLog:     config.Ptr(false),
	}}
	cleanup := initLogging(cfg, processStart)
	t.Cleanup(func() {
		cleanup()
		_ = log.Init(log.Config{}) // back to stderr-only for later tests
	})

	live, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(live), early) {
		t.Errorf("this lifetime's early WARN was archived away; live log:\n%s", live)
	}
	if strings.Contains(string(live), outgoing) {
		t.Errorf("previous lifetime's line survived the startup rotation; live log:\n%s", live)
	}

	archived := readArchives(t, archiveDir)
	if !strings.Contains(archived, outgoing) {
		t.Errorf("previous lifetime's line missing from archive; archive:\n%s", archived)
	}
	if strings.Contains(archived, early) {
		t.Errorf("this lifetime's early WARN reached the archive; archive:\n%s", archived)
	}
}

func readArchives(t *testing.T, dir string) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.gz"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, p := range paths {
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(f)
		if err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		data, err := io.ReadAll(zr)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		b.Write(data)
	}
	return b.String()
}
