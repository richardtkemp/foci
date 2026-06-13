package log

import (
	"bytes"
	"io"
	"os"
	"testing"
)

// setLevel changes the log level at runtime (test-only).
func setLevel(level Level) {
	std.mu.Lock()
	std.level = level
	std.mu.Unlock()
}

// getLevel returns the current log level (test-only).
func getLevel() Level {
	std.mu.Lock()
	defer std.mu.Unlock()
	return std.level
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
	std.mu.Lock()
	std.level = INFO
	std.eventOut = os.Stderr
	std.apiFile = nil
	std.payloadFile = nil
	std.buffer = nil
	std.initialized = false
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
