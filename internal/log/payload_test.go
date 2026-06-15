package log

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemHash(t *testing.T) {
	// Verifies that SystemHash returns a stable 16-char hex digest
	// for the same input and an empty string for nil/empty input.
	if got := SystemHash(nil); got != "" {
		t.Errorf("SystemHash(nil) = %q, want empty", got)
	}
	if got := SystemHash([]string{}); got != "" {
		t.Errorf("SystemHash([]) = %q, want empty", got)
	}

	h1 := SystemHash([]string{"Hello", "World"})
	if len(h1) != 16 {
		t.Errorf("SystemHash length = %d, want 16", len(h1))
	}
	// Same input produces same hash.
	h2 := SystemHash([]string{"Hello", "World"})
	if h1 != h2 {
		t.Errorf("SystemHash not stable: %q != %q", h1, h2)
	}
	// Different input produces different hash.
	h3 := SystemHash([]string{"Hello", "World!"})
	if h1 == h3 {
		t.Errorf("SystemHash collision: %q == %q for different inputs", h1, h3)
	}
}

func TestPayloadEnabled(t *testing.T) {
	// Verifies that PayloadEnabled returns false when no payload file is
	// configured, and true after Init with a PayloadFile path.
	resetGlobal()
	t.Cleanup(resetGlobal)

	if PayloadEnabled() {
		t.Error("PayloadEnabled() should be false with no payload file")
	}

	dir := t.TempDir()

	// PayloadFile set but full_payload off → still disabled (the file path has
	// a non-empty default, so full_payload is the real opt-in gate).
	if err := Init(Config{
		Level:       "INFO",
		PayloadFile: filepath.Join(dir, "payload.jsonl"),
	}); err != nil {
		t.Fatalf("Init (gate off): %v", err)
	}
	if PayloadEnabled() {
		t.Error("PayloadEnabled() should be false when full_payload is unset")
	}
	Close()

	err := Init(Config{
		Level:       "INFO",
		PayloadFile: filepath.Join(dir, "payload.jsonl"),
		FullPayload: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	if !PayloadEnabled() {
		t.Error("PayloadEnabled() should be true after Init with PayloadFile + full_payload")
	}
}

func TestPayloadLog(t *testing.T) {
	// Verifies that Payload() serializes a PayloadEntry to the payload file
	// with session and model fields present.
	resetGlobal()
	t.Cleanup(resetGlobal)

	dir := t.TempDir()
	payloadPath := filepath.Join(dir, "payload.jsonl")

	err := Init(Config{
		Level:       "INFO",
		PayloadFile: payloadPath,
		FullPayload: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer Close()

	sysHash := SystemHash([]string{"You are a helper."})
	Payload(PayloadEntry{
		Session:    "test-session",
		SeqNum:     3,
		Model:      "test-model",
		SystemHash: sysHash,
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
	line := string(data)
	for _, want := range []string{"test-session", "test-model", `"seq":3`, sysHash} {
		if !strings.Contains(line, want) {
			t.Errorf("payload missing %q: %s", want, line)
		}
	}
}

func TestPayload(t *testing.T) {
	// Verifies that Payload() does not panic when payloadFile is nil.
	resetGlobal()
	t.Cleanup(resetGlobal)

	Payload(PayloadEntry{
		Session: "test",
		Model:   "test-model",
	})
	// No panic = pass
}

func TestPayloadNoFileNoOp(t *testing.T) {
	// Verifies that the internal payload method is a no-op
	// (no panic) when payloadFile is nil.
	resetGlobal()
	t.Cleanup(resetGlobal)

	std.payload(PayloadEntry{Session: "test", Model: "test"})
}
