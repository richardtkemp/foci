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

// TestAPIProviderInferenceSQLite verifies the provider column is populated in the SQLite
// api_calls table for all model families, including Anthropic models (the prior bug was
// that claude-* models had an empty provider).
func TestAPIProviderInferenceSQLite(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	tests := []struct {
		model    string
		wantProv string
	}{
		{"claude-haiku-4-5", "anthropic"},
		{"claude-opus-4-6", "anthropic"},
		{"claude-sonnet-4-5", "anthropic"},
		{"gemini-2.5-pro", "gemini"},
		{"gemini-2.5-flash", "gemini"},
		{"gpt-4", "openai"},
		{"o3", "openai"},
	}

	for _, tt := range tests {
		API(APIEntry{Session: "test-" + tt.model, Model: tt.model, CallType: "conversation"})
	}

	for _, tt := range tests {
		var provider string
		err := apiLog.db.QueryRow("SELECT provider FROM api_calls WHERE session = ?", "test-"+tt.model).Scan(&provider)
		if err != nil {
			t.Fatalf("query provider for model %q: %v", tt.model, err)
		}
		if provider != tt.wantProv {
			t.Errorf("model %q: SQLite provider = %q, want %q", tt.model, provider, tt.wantProv)
		}
	}
}

// TestAPIProviderExplicitOverridesInference verifies that an explicitly set Provider field
// on the APIEntry is preserved and not overwritten by inference.
func TestAPIProviderExplicitOverridesInference(t *testing.T) {
	resetGlobal()
	t.Cleanup(resetGlobal)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	// Explicitly set provider to "openai" even though model looks like Anthropic.
	// This simulates an OpenAI-compatible endpoint serving a claude model.
	API(APIEntry{
		Session:  "explicit",
		Model:    "claude-haiku-4-5",
		Provider: "openai",
		CallType: "conversation",
	})

	var provider string
	err := apiLog.db.QueryRow("SELECT provider FROM api_calls WHERE session = 'explicit'").Scan(&provider)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if provider != "openai" {
		t.Errorf("explicit provider not preserved: got %q, want %q", provider, "openai")
	}
}

// TestAPIProviderInference verifies that the Provider field is auto-inferred from the model name:
// gemini models get "gemini", openai models get "openai", claude models get "anthropic".
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
		{"claude-haiku-4-5", "anthropic"},
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
