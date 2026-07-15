package config

import (
	"testing"

	"foci/internal/modelinfo"
)

func TestModelInfoEntryToModel_NewModel(t *testing.T) {
	modelinfo.ResetToBuiltIn()

	ctx := 300_000
	in, out := 2.0, 10.0
	effort, thinking := true, true
	entry := ModelInfoEntry{
		ID:            "test-new-model",
		ContextWindow: &ctx,
		InputPer1M:    &in,
		OutputPer1M:   &out,
		CanEffort:     &effort,
		CanThinking:   &thinking,
	}

	m, err := entry.toModel()
	if err != nil {
		t.Fatalf("toModel: %v", err)
	}

	if m.ContextWindow != ctx {
		t.Errorf("ContextWindow = %d, want %d", m.ContextWindow, ctx)
	}
	if m.InputPer1M != in {
		t.Errorf("InputPer1M = %v, want %v", m.InputPer1M, in)
	}
	if !m.Effort {
		t.Error("Effort should be true")
	}
	if !m.Thinking {
		t.Error("Thinking should be true")
	}
	// Defaults: unspecified can_* should be false, cache pricing 0.0.
	if m.Speed || m.Caching {
		t.Error("Speed/Caching should default to false")
	}
	if m.CacheReadPer1M != 0 || m.CacheWritePer1M != 0 {
		t.Error("Cache pricing should default to 0.0")
	}
}

func TestModelInfoEntryToModel_NewModelMissingRequired(t *testing.T) {
	modelinfo.ResetToBuiltIn()

	ctx := 300_000
	entry := ModelInfoEntry{
		ID:            "test-incomplete",
		ContextWindow: &ctx,
		// Missing input_per_1m and output_per_1m
	}

	_, err := entry.toModel()
	if err == nil {
		t.Fatal("expected error for missing required fields, got nil")
	}
}

func TestModelInfoEntryToModel_NewModelCanDefaults(t *testing.T) {
	modelinfo.ResetToBuiltIn()

	ctx := 300_000
	in, out := 2.0, 10.0
	entry := ModelInfoEntry{
		ID:            "test-defaults",
		ContextWindow: &ctx,
		InputPer1M:    &in,
		OutputPer1M:   &out,
		// No can_* fields at all
	}

	m, err := entry.toModel()
	if err != nil {
		t.Fatalf("toModel: %v", err)
	}

	if m.Effort || m.Thinking || m.Speed || m.Caching {
		t.Error("all capabilities should default to false for new model")
	}
}

func TestModelInfoEntryToModel_EmptyID(t *testing.T) {
	_, err := ModelInfoEntry{}.toModel()
	if err == nil {
		t.Fatal("expected error for empty id, got nil")
	}
}

func TestModelInfoEntryToModel_PartialOverride(t *testing.T) {
	modelinfo.ResetToBuiltIn()

	// Get built-in haiku values to compare against.
	original, _ := modelinfo.Lookup("claude-haiku-4-5")

	// Override only pricing.
	newIn, newOut := 0.50, 2.50
	entry := ModelInfoEntry{
		ID:          "claude-haiku-4-5",
		InputPer1M:  &newIn,
		OutputPer1M: &newOut,
	}

	m, err := entry.toModel()
	if err != nil {
		t.Fatalf("toModel: %v", err)
	}

	// Overridden fields.
	if m.InputPer1M != newIn {
		t.Errorf("InputPer1M = %v, want %v", m.InputPer1M, newIn)
	}
	if m.OutputPer1M != newOut {
		t.Errorf("OutputPer1M = %v, want %v", m.OutputPer1M, newOut)
	}

	// Preserved fields from the built-in entry.
	if m.ContextWindow != original.ContextWindow {
		t.Errorf("ContextWindow = %d, want %d (should be preserved from built-in)", m.ContextWindow, original.ContextWindow)
	}
	if m.Caching != original.Caching {
		t.Errorf("Caching = %v, want %v (should be preserved from built-in)", m.Caching, original.Caching)
	}
	if m.CacheReadPer1M != original.CacheReadPer1M {
		t.Errorf("CacheReadPer1M = %v, want %v (should be preserved from built-in)", m.CacheReadPer1M, original.CacheReadPer1M)
	}
}

func TestApplyModelInfo(t *testing.T) {
	modelinfo.ResetToBuiltIn()

	ctx := 500_000
	in, out := 1.5, 7.5
	entries := []ModelInfoEntry{
		{
			ID:            "apply-test-1",
			ContextWindow: &ctx,
			InputPer1M:    &in,
			OutputPer1M:   &out,
		},
		{
			ID: "apply-test-2",
			// Missing required fields — should be skipped with warning.
		},
	}

	ApplyModelInfo(entries)

	// First entry should be registered.
	m, ok := modelinfo.Lookup("apply-test-1")
	if !ok {
		t.Fatal("apply-test-1 not found in registry")
	}
	if m.ContextWindow != ctx {
		t.Errorf("ContextWindow = %d, want %d", m.ContextWindow, ctx)
	}

	// Second entry should NOT be registered (missing required fields).
	if _, ok := modelinfo.Lookup("apply-test-2"); ok {
		t.Error("apply-test-2 should have been skipped (missing required fields)")
	}
}

func TestApplyModelInfo_MultipleEntries(t *testing.T) {
	modelinfo.ResetToBuiltIn()

	ctx1, ctx2 := 200_000, 400_000
	in1, out1 := 1.0, 5.0
	in2, out2 := 2.0, 10.0
	entries := []ModelInfoEntry{
		{ID: "multi-1", ContextWindow: &ctx1, InputPer1M: &in1, OutputPer1M: &out1},
		{ID: "multi-2", ContextWindow: &ctx2, InputPer1M: &in2, OutputPer1M: &out2},
	}

	ApplyModelInfo(entries)

	for _, id := range []string{"multi-1", "multi-2"} {
		if _, ok := modelinfo.Lookup(id); !ok {
			t.Errorf("%s not found in registry", id)
		}
	}
}
