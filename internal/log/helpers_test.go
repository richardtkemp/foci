package log

import (
	"bytes"
	"io"
	"os"
	"testing"
	"time"
)

// setLevel changes the log level at runtime (test-only).
func setLevel(level Level) {
	std.level.Store(int32(level))
}

// getLevel returns the current log level (test-only).
func getLevel() Level {
	return Level(std.level.Load())
}

// setOutput replaces the event output writer (test-only).
func setOutput(w io.Writer) {
	std.mu.Lock()
	std.eventOut = w
	std.mu.Unlock()
}

// filePaths returns the configured log file paths (test-only).
func filePaths() (event, api, payload string) {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.eventPath, std.apiPath, std.payloadPath
}

// initConversation opens a single conversation log (test-only).
// resetGlobal restores the global logger to its initial state for test isolation.
func resetGlobal() {
	std.level.Store(int32(INFO))
	std.mu.Lock()
	std.eventOut = os.Stderr
	std.apiFile = nil
	std.payloadFile = nil
	std.buffer = nil
	std.initialized = false
	std.lastEventStaleWarn = time.Time{}
	std.lastAPIStaleWarn = time.Time{}
	std.lastPayloadStaleWarn = time.Time{}
	std.mu.Unlock()
}

// captureOutput redirects the logger to a buffer and registers cleanup to restore stderr.
func captureOutput(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	setOutput(&buf)
	t.Cleanup(func() { setOutput(os.Stderr) })
	return &buf
}

// withDebugLevel sets the log level to DEBUG and registers cleanup to restore INFO.
func withDebugLevel(t *testing.T) {
	t.Helper()
	setLevel(DEBUG)
	t.Cleanup(func() { setLevel(INFO) })
}

// resetExtraLogging clears the process-global "xtra:" per-package enabled set
// and registers cleanup to restore whatever was there before. Without this,
// EnableExtra/SetExtra calls made by one test persist in the package-level
// atomic for the rest of the process — including into a later `-count>1`
// rerun of the same test binary, where a test asserting "disabled by
// default" would otherwise find its component already enabled by its own
// prior run.
func resetExtraLogging(t *testing.T) {
	t.Helper()
	prev := extraLogging.Load()
	extraLogging.Store(nil)
	t.Cleanup(func() { extraLogging.Store(prev) })
}

// openAPIWriter opens a file as the API JSONL writer and registers cleanup.
func openAPIWriter(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatal(err)
	}
	SetAPIWriter(f)
	t.Cleanup(func() { SetAPIWriter(nil); f.Close() })
	return f
}
