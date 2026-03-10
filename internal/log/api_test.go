package log

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAPILog verifies that a well-formed APIEntry is serialized as JSONL to the API
// writer, with all fields correctly round-tripped through JSON.
func TestAPILog(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")
	f := openAPIWriter(t, path)

	entry := APIEntry{
		Timestamp:  time.Date(2026, 2, 21, 3, 52, 41, 0, time.UTC),
		Session:    "agent:main:main",
		Model:      "claude-haiku-4-5",
		Input:      1119,
		Output:     164,
		CacheRead:  0,
		CacheWrite: 1119,
		CostUSD:    0.003,
		DurationMS: 1240,
	}

	API(entry)

	// Close and re-read
	f.Close()
	data, _ := os.ReadFile(path)

	var decoded APIEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal API entry: %v\nraw: %s", err, string(data))
	}

	if decoded.Session != "agent:main:main" {
		t.Errorf("Session = %q", decoded.Session)
	}
	if decoded.Model != "claude-haiku-4-5" {
		t.Errorf("Model = %q", decoded.Model)
	}
	if decoded.Input != 1119 {
		t.Errorf("Input = %d", decoded.Input)
	}
	if decoded.Output != 164 {
		t.Errorf("Output = %d", decoded.Output)
	}
	if decoded.CacheWrite != 1119 {
		t.Errorf("CacheWrite = %d", decoded.CacheWrite)
	}
	if decoded.DurationMS != 1240 {
		t.Errorf("DurationMS = %d", decoded.DurationMS)
	}
}

// TestAPILogDisabled verifies that API() is a no-op (no panic) when no API writer is set.
func TestAPILogDisabled(t *testing.T) {
	SetAPIWriter(nil)
	API(APIEntry{Session: "test"})
	// No panic = pass
}

// TestMultipleAPIEntries verifies that multiple API() calls produce one JSONL line each,
// all appended to the same file.
func TestMultipleAPIEntries(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")
	f := openAPIWriter(t, path)

	for i := 0; i < 3; i++ {
		API(APIEntry{Session: "test", DurationMS: int64(i * 100)})
	}

	f.Close()
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Errorf("got %d lines, want 3", len(lines))
	}
}

// TestAPIWithGemini verifies that API() handles a Gemini model without panicking,
// including provider auto-inference from the model name.
func TestAPIWithGemini(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	API(APIEntry{
		Session:  "test",
		Model:    "gemini-2-flash",
		CallType: "conversation",
	})
	// No error = pass
}

// TestAPIWithOpenAI verifies that API() handles an OpenAI model without panicking,
// including provider auto-inference from the model name.
func TestAPIWithOpenAI(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	API(APIEntry{
		Session:  "test",
		Model:    "gpt-4",
		CallType: "conversation",
	})
	// No error = pass
}

// TestAPIDefaultCallType verifies that an APIEntry with empty CallType is written
// with CallType defaulting to "conversation".
func TestAPIDefaultCallType(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")
	f := openAPIWriter(t, path)

	API(APIEntry{Session: "test", Model: "test"})

	f.Close()
	data, _ := os.ReadFile(path)
	var decoded APIEntry
	json.Unmarshal(data, &decoded)
	if decoded.CallType != "conversation" {
		t.Errorf("default CallType = %q, want %q", decoded.CallType, "conversation")
	}
}

// TestAPIProviderInference verifies that the Provider field is auto-inferred from the model name:
// gemini models get "gemini", gpt models get "openai", claude models get "".
func TestAPIProviderInference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")
	f := openAPIWriter(t, path)

	tests := []struct {
		model    string
		wantProv string
	}{
		{"gemini-2.5-pro", "gemini"},
		{"gpt-4", "openai"},
		{"claude-haiku-4-5", ""},
	}

	for _, tt := range tests {
		f.Truncate(0)
		f.Seek(0, 0)

		API(APIEntry{Session: "test", Model: tt.model})

		f.Sync()
		data, _ := os.ReadFile(path)
		var decoded APIEntry
		json.Unmarshal(bytes.TrimSpace(data), &decoded)
		if decoded.Provider != tt.wantProv {
			t.Errorf("model %q → provider = %q, want %q", tt.model, decoded.Provider, tt.wantProv)
		}
	}
}
