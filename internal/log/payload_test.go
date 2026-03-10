package log

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPayloadEnabled verifies that PayloadEnabled returns false when no payload file is
// configured, and true after Init with a PayloadFile path.
func TestPayloadEnabled(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	if PayloadEnabled() {
		t.Error("PayloadEnabled() should be false with no payload file")
	}

	dir := t.TempDir()
	err := Init(Config{
		Level:       "INFO",
		PayloadFile: filepath.Join(dir, "payload.jsonl"),
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	if !PayloadEnabled() {
		t.Error("PayloadEnabled() should be true after Init with PayloadFile")
	}
}

// TestPayloadLog verifies that Payload() serializes a PayloadEntry to the payload file
// with session and model fields present.
func TestPayloadLog(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{
		Level:       "INFO",
		PayloadFile: payloadPath,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	Payload(PayloadEntry{
		Session:    "test-session",
		Model:      "test-model",
		Request:    json.RawMessage(`{"prompt":"hello"}`),
		Response:   json.RawMessage(`{"text":"world"}`),
		DurationMS: 500,
	})

	// Force close to flush
	Close()

	data, err := os.ReadFile(payloadPath)
	if err != nil {
		t.Fatalf("read payload: %v", err)
	}
	if !strings.Contains(string(data), "test-session") {
		t.Errorf("payload missing session: %s", string(data))
	}
	if !strings.Contains(string(data), "test-model") {
		t.Errorf("payload missing model: %s", string(data))
	}
}

// TestPayload verifies that Payload() does not panic when payloadFile is nil.
func TestPayload(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	Payload(PayloadEntry{
		Session: "test",
		Model:   "test-model",
	})
	// No panic = pass
}

// TestPayloadNoFileNoOp verifies that the internal payload method is a no-op
// (no panic) when payloadFile is nil.
func TestPayloadNoFileNoOp(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	std.payload(PayloadEntry{Session: "test", Model: "test"})
}
