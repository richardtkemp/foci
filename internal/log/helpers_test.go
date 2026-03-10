package log

import (
	"bytes"
	"os"
	"testing"
)

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
	SetOutput(&buf)
	t.Cleanup(func() { SetOutput(os.Stderr) })
	return &buf
}

// withDebugLevel sets the log level to DEBUG and registers cleanup to restore INFO.
func withDebugLevel(t *testing.T) {
	t.Helper()
	SetLevel(DEBUG)
	t.Cleanup(func() { SetLevel(INFO) })
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
