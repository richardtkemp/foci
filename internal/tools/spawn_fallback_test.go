package tools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/provider"
)

// TestSpawnOneShotBooksServedFallbackTuple proves a one-shot spawn a fallback
// hop served books its ledger row on the SERVING tuple — Provider and Model
// name the hop's format and canonical model — not the requested primary.
// Not parallel: it makes a ledger the live one.
func TestSpawnOneShotBooksServedFallbackTuple(t *testing.T) {
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })

	primary := &fallbackProbeClient{responses: []fallbackProbeResponse{
		{err: &provider.APIError{StatusCode: 503, Body: "unavailable"}},
	}}
	fallback := &fallbackProbeClient{responses: []fallbackProbeResponse{
		{text: "spawned answer"},
	}}
	deps := SpawnDeps{
		Client:         primary,
		FallbackModel:  "anthropic/claude-haiku-4-5",
		FallbackFormat: "anthropic",
		FallbackFunc:   openAIFallbackFrom("anthropic/claude-haiku-4-5"),
		ClientProvider: probeClientProvider{clients: map[string]provider.Client{
			"openai/openai": fallback,
		}},
		MaxToolLoops: func() int { return 3 },
	}

	params, _ := json.Marshal(map[string]string{"prompt": "test", "context": "character"})
	if _, err := NewSpawnTool(deps, nil).Execute(WithSessionKey(context.Background(), "gil/cfb"), params); err != nil {
		t.Fatalf("spawn: %v", err)
	}

	rows, err := l.Calls(time.Time{})
	if err != nil || len(rows) != 1 {
		t.Fatalf("ledger calls = %d (%v), want the one spawn call", len(rows), err)
	}
	got := rows[0]
	if got.Kind != accounting.KindSpawn {
		t.Errorf("call kind = %q, want %q", got.Kind, accounting.KindSpawn)
	}
	if got.Provider != "openai" || got.Model != "openai/gpt-5.6" {
		t.Errorf("booked Provider/Model = %q/%q, want openai / openai/gpt-5.6", got.Provider, got.Model)
	}
}
