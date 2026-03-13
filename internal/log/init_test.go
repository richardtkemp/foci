package log

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInitWithFiles(t *testing.T) {
	// Verifies that Init opens event and API log files, and that
	// log calls write to those files on disk.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	apiPath := filepath.Join(dir, "api.jsonl")

	err := Init(Config{
		Level:     "DEBUG",
		EventFile: eventPath,
		APIFile:   apiPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	Infof("test", "hello from init test")
	API(APIEntry{Session: "init-test", Model: "test", DurationMS: 100})

	// Event log should exist on disk
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	if !strings.Contains(string(data), "hello from init test") {
		t.Errorf("event log missing message: %s", string(data))
	}

	// API log should exist on disk
	data, err = os.ReadFile(apiPath)
	if err != nil {
		t.Fatalf("read api log: %v", err)
	}
	if !strings.Contains(string(data), "init-test") {
		t.Errorf("api log missing entry: %s", string(data))
	}
}

func TestInitBadEventPath(t *testing.T) {
	// Verifies Init returns an error when the event file path is invalid.
	err := Init(Config{EventFile: "/nonexistent/dir/foci.log"})
	if err == nil {
		t.Fatal("expected error for bad event file path")
	}
}

func TestInitBadAPIPath(t *testing.T) {
	// Verifies Init returns an error when the API file path is invalid.
	err := Init(Config{APIFile: "/nonexistent/dir/api.jsonl"})
	if err == nil {
		t.Fatal("expected error for bad API file path")
	}
}

func TestInitBadPayloadPath(t *testing.T) {
	// Verifies Init returns an error for a bad payload file path.
	resetGlobal()
	t.Cleanup(resetGlobal)

	err := Init(Config{PayloadFile: "/nonexistent/dir/payload.jsonl"})
	if err == nil {
		t.Fatal("expected error for bad payload file path")
	}
}

func TestFilePaths(t *testing.T) {
	// Verifies that FilePaths returns the exact paths passed to Init
	// for the event, API, and payload log files.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	apiPath := filepath.Join(dir, "api.jsonl")
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{
		Level:       "INFO",
		EventFile:   eventPath,
		APIFile:     apiPath,
		PayloadFile: payloadPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	gotEvent, gotAPI, gotPayload := FilePaths()
	if gotEvent != eventPath {
		t.Errorf("event path = %q, want %q", gotEvent, eventPath)
	}
	if gotAPI != apiPath {
		t.Errorf("api path = %q, want %q", gotAPI, apiPath)
	}
	if gotPayload != payloadPath {
		t.Errorf("payload path = %q, want %q", gotPayload, payloadPath)
	}
}

func TestInitEventFileOpenError(t *testing.T) {
	// Verifies Init returns an error when the event file path
	// is an existing directory (OpenFile on a directory fails).
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	badPath := filepath.Join(dir, "foci.log")
	os.MkdirAll(badPath, 0755)

	err := Init(Config{EventFile: badPath})
	if err == nil {
		Close()
		t.Fatal("expected error when event path is a directory")
	}
}

func TestInitAPIFileOpenError(t *testing.T) {
	// Verifies Init returns an error when the API file can't be opened
	// (e.g. the path is an existing directory).
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	badPath := filepath.Join(dir, "api.jsonl")
	os.MkdirAll(badPath, 0755)

	err := Init(Config{APIFile: badPath})
	if err == nil {
		Close()
		t.Fatal("expected error when API path is a directory")
	}
}

func TestInitPayloadFileOpenError(t *testing.T) {
	// Verifies Init returns an error when the payload file can't be opened.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	badPath := filepath.Join(dir, "payload.jsonl")
	os.MkdirAll(badPath, 0755)

	err := Init(Config{PayloadFile: badPath})
	if err == nil {
		Close()
		t.Fatal("expected error when payload path is a directory")
	}
}

func TestReopenAllFiles(t *testing.T) {
	// Verifies that Reopen closes and reopens all three log file types,
	// and that writes before and after reopen both appear in the files.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")
	apiPath := filepath.Join(dir, "api.jsonl")
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{
		Level:       "INFO",
		EventFile:   eventPath,
		APIFile:     apiPath,
		PayloadFile: payloadPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	// Write before reopen
	Infof("test", "before reopen")
	API(APIEntry{Session: "test", Model: "test"})
	Payload(PayloadEntry{Session: "test", Model: "test"})

	if err := Reopen(); err != nil {
		t.Fatalf("Reopen: %v", err)
	}

	// Write after reopen — should succeed to new file handles
	Infof("test", "after reopen")
	API(APIEntry{Session: "test2", Model: "test"})
	Payload(PayloadEntry{Session: "test2", Model: "test"})

	// Force close to flush
	Close()

	// Verify content persists in all files
	eventData, _ := os.ReadFile(eventPath)
	if !strings.Contains(string(eventData), "after reopen") {
		t.Errorf("event log missing post-reopen message")
	}
	apiData, _ := os.ReadFile(apiPath)
	if !strings.Contains(string(apiData), "test2") {
		t.Errorf("api log missing post-reopen entry")
	}
	payloadData, _ := os.ReadFile(payloadPath)
	if !strings.Contains(string(payloadData), "test2") {
		t.Errorf("payload log missing post-reopen entry")
	}
}

func TestReopenNoFiles(t *testing.T) {
	// Verifies that Reopen is a no-op (no error) when no files are open.
	resetGlobal()
	t.Cleanup(resetGlobal)

	if err := Reopen(); err != nil {
		t.Fatalf("Reopen with no files: %v", err)
	}
}

func TestReopenEventError(t *testing.T) {
	// Verifies Reopen returns an error when the event file can't be reopened.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")

	err := Init(Config{Level: "INFO", EventFile: eventPath})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	// Point event path to a non-writable location
	std.mu.Lock()
	std.eventPath = "/nonexistent/dir/foci.log"
	std.mu.Unlock()

	if err := Reopen(); err == nil {
		t.Fatal("expected error reopening event file with bad path")
	}
}

func TestReopenAPIError(t *testing.T) {
	// Verifies Reopen returns an error when the API file can't be reopened.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	apiPath := filepath.Join(dir, "api.jsonl")

	err := Init(Config{Level: "INFO", APIFile: apiPath})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	std.mu.Lock()
	std.apiPath = "/nonexistent/dir/api.jsonl"
	std.mu.Unlock()

	if err := Reopen(); err == nil {
		t.Fatal("expected error reopening API file with bad path")
	}
}

func TestReopenPayloadError(t *testing.T) {
	// Verifies Reopen returns an error when the payload file can't be reopened.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{Level: "INFO", PayloadFile: payloadPath})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	std.mu.Lock()
	std.payloadPath = "/nonexistent/dir/payload.jsonl"
	std.mu.Unlock()

	if err := Reopen(); err == nil {
		t.Fatal("expected error reopening payload file with bad path")
	}
}

func TestPreInitBufferReplay(t *testing.T) {
	// Verifies that messages logged before Init are buffered,
	// written to stderr immediately, and then replayed to the event file when Init runs.
	// The buffer is cleared after Init and post-Init messages are not buffered.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	eventPath := filepath.Join(dir, "foci.log")

	// Log before Init — should go to stderr (captured by SetOutput)
	// and be buffered for replay.
	var stderrBuf bytes.Buffer
	SetOutput(&stderrBuf)

	Warnf("config", "unknown key: foo.bar")
	Infof("startup", "loading config from foci.toml")

	// Verify buffer has two entries
	std.mu.Lock()
	bufLen := len(std.buffer)
	std.mu.Unlock()
	if bufLen != 2 {
		t.Fatalf("buffer len = %d, want 2", bufLen)
	}

	// Verify stderr got the messages
	if !strings.Contains(stderrBuf.String(), "unknown key: foo.bar") {
		t.Errorf("stderr missing pre-Init warning: %q", stderrBuf.String())
	}

	// Now Init — should replay buffered events to the event file
	err := Init(Config{
		Level:     "DEBUG",
		EventFile: eventPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	// Event file should contain the replayed pre-Init messages
	data, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatalf("read event log: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, "unknown key: foo.bar") {
		t.Errorf("event file missing replayed warning: %s", content)
	}
	if !strings.Contains(content, "loading config from foci.toml") {
		t.Errorf("event file missing replayed info: %s", content)
	}

	// Buffer should be cleared after Init
	std.mu.Lock()
	bufLen = len(std.buffer)
	std.mu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should be cleared after Init, got %d entries", bufLen)
	}

	// Post-Init messages should NOT be buffered
	Infof("test", "post-init message")
	std.mu.Lock()
	bufLen = len(std.buffer)
	std.mu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should stay empty after Init, got %d entries", bufLen)
	}
}

func TestPreInitBufferNoFile(t *testing.T) {
	// Verifies that the pre-Init buffer is cleared after Init
	// even when no event file is configured (no replay occurs, buffer just cleared).
	resetGlobal()
	t.Cleanup(resetGlobal)

	captureOutput(t)

	Warnf("test", "pre-init warning")

	// Init without an event file — buffer is cleared but not replayed
	err := Init(Config{Level: "INFO"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	std.mu.Lock()
	bufLen := len(std.buffer)
	std.mu.Unlock()
	if bufLen != 0 {
		t.Errorf("buffer should be cleared after Init, got %d entries", bufLen)
	}
}

func TestPreInitFilteredByLevel(t *testing.T) {
	// Verifies that messages below the configured level
	// are not added to the pre-Init buffer (DEBUG is filtered at the default INFO level).
	resetGlobal()
	t.Cleanup(resetGlobal)

	captureOutput(t)

	Debugf("test", "debug before init")
	Infof("test", "info before init")

	std.mu.Lock()
	bufLen := len(std.buffer)
	std.mu.Unlock()
	if bufLen != 1 {
		t.Fatalf("buffer len = %d, want 1 (DEBUG filtered by INFO level)", bufLen)
	}
}
