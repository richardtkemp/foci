package log

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"foci/internal/modelinfo"
)

func TestAPILog(t *testing.T) {
	// Verifies that a well-formed APIEntry is serialized as JSONL to the API
	// writer, with all fields correctly round-tripped through JSON.
	dir := t.TempDir()
	path := filepath.Join(dir, "api.jsonl")
	f := openAPIWriter(t, path)

	entry := APIEntry{
		Timestamp:     time.Date(2026, 2, 21, 3, 52, 41, 0, time.UTC),
		Session:       "main/i0/0",
		Model:         "claude-haiku-4-5",
		Input:         1119,
		Output:        164,
		CacheRead:     0,
		CacheWrite:    1119,
		GoldenCostUSD: f64p(0.003),
		DurationMS:    1240,
	}

	API(entry)

	// Close and re-read
	f.Close()
	data, _ := os.ReadFile(path)

	var decoded APIEntry
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal API entry: %v\nraw: %s", err, string(data))
	}

	if decoded.Session != "main/i0/0" {
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

func TestAPILogDisabled(t *testing.T) {
	// Verifies that API() is a no-op (no panic) when no API writer is set.
	SetAPIWriter(nil)
	API(APIEntry{Session: "test"})
	// No panic = pass
}

func TestMultipleAPIEntries(t *testing.T) {
	// Verifies that multiple API() calls produce one JSONL line each,
	// all appended to the same file.
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

func TestAPIDefaultCallType(t *testing.T) {
	// Verifies that an APIEntry with empty CallType is written
	// with CallType defaulting to "conversation".
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

func TestAPIProviderInferenceSQLite(t *testing.T) {
	// Verifies the provider column is populated in the SQLite
	// api_calls table for all model families, including Anthropic models (the prior bug was
	// that claude-* models had an empty provider).
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

func TestQuerySessionStatsContextTokensSkipsSynthetic(t *testing.T) {
	// A synthetic turn (no-inference, zero tokens) landing on top of a real
	// turn must not zero out ContextTokens — otherwise the /status Context
	// line is suppressed even though the session holds a full context window.
	resetGlobal()
	t.Cleanup(resetGlobal)

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "api.db")
	if err := InitAPIDB(dbPath); err != nil {
		t.Fatalf("InitAPIDB: %v", err)
	}
	defer CloseAPIDB()

	sess := "clutch/ctxtest"
	base := time.Date(2026, 7, 14, 10, 0, 0, 0, time.UTC)

	// Real delegated turn with a full context window...
	API(APIEntry{
		Timestamp:  base,
		Session:    sess,
		Model:      "claude-opus-4-8",
		CallType:   "delegated_turn",
		Input:      2,
		CacheRead:  163681,
		CacheWrite: 615,
	})
	// ...then two synthetic no-inference turns on top (zero tokens).
	API(APIEntry{
		Timestamp: base.Add(1 * time.Minute),
		Session:   sess,
		Model:     "<synthetic>",
		CallType:  "delegated_turn",
	})
	API(APIEntry{
		Timestamp: base.Add(2 * time.Minute),
		Session:   sess,
		Model:     "<synthetic>",
		CallType:  "delegated_turn",
	})

	stats, err := QuerySessionStats(sess)
	if err != nil {
		t.Fatalf("QuerySessionStats: %v", err)
	}
	if want := 2 + 163681 + 615; stats.ContextTokens != want {
		t.Errorf("ContextTokens = %d, want %d (should reflect the last real turn, not the synthetic one)", stats.ContextTokens, want)
	}
}

func TestAPIProviderExplicitOverridesInference(t *testing.T) {
	// Verifies that an explicitly set Provider field
	// on the APIEntry is preserved and not overwritten by inference.
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

func TestAPIProviderInference(t *testing.T) {
	// Verifies that the Provider field is auto-inferred from the model name:
	// gemini models get "gemini", openai models get "openai", claude models get "anthropic".
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

// f64p returns a pointer to f — helper for building APIEntry.GoldenCostUSD
// test fixtures (a float literal isn't addressable directly).
func f64p(f float64) *float64 { return &f }

// TestEffectiveCostGoldenVerbatim verifies a golden (provider-reported) cost
// is returned exactly as stored — never recomputed — even if it disagrees
// with what a flat modelinfo calculation from the same tokens would give
// (foci_todo #1407).
func TestEffectiveCostGoldenVerbatim(t *testing.T) {
	golden := 0.12345
	e := APIEntry{
		Timestamp:     time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Model:         "claude-haiku-4-5",
		Input:         1_000_000,
		Output:        1_000_000,
		GoldenCostUSD: &golden,
	}
	if got := e.EffectiveCost(); got != golden {
		t.Errorf("EffectiveCost() = %v, want golden value %v verbatim", got, golden)
	}
}

// TestEffectiveCostLiveWhenGoldenAbsent verifies a nil GoldenCostUSD (no
// provider-reported cost) falls back to a live calculation from the stored
// tokens, rather than staying zero or persisting anything.
func TestEffectiveCostLiveWhenGoldenAbsent(t *testing.T) {
	e := APIEntry{
		Timestamp: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC),
		Model:     "claude-haiku-4-5",
		Input:     1_000_000,
		Output:    500_000,
	}
	want := modelinfo.CostAsOf(e.Model, e.Timestamp, e.Input, e.Output, e.CacheRead, e.CacheWrite)
	if want <= 0 {
		t.Fatal("test precondition: expected a positive live-calculated cost for claude-haiku-4-5")
	}
	if got := e.EffectiveCost(); got != want {
		t.Errorf("EffectiveCost() = %v, want live-calculated %v", got, want)
	}
	if e.GoldenCostUSD != nil {
		t.Error("GoldenCostUSD should stay nil — EffectiveCost must not mutate/persist a calculated value")
	}
}
