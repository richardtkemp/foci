package tools

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"foci/internal/config"
	"foci/internal/delegator/accounting"
	"foci/internal/provider"
)

// openSummaryLedger makes a fresh ledger the live one for the test.
func openSummaryLedger(t *testing.T) *accounting.Ledger {
	t.Helper()
	l, _, err := accounting.Open(filepath.Join(t.TempDir(), "api.db"), accounting.Options{})
	if err != nil {
		t.Fatal(err)
	}
	accounting.SetLive(l)
	t.Cleanup(func() { accounting.SetLive(nil); _ = l.Close() })
	return l
}

// summaryCalls reads every call booked in l.
func summaryCalls(t *testing.T, l *accounting.Ledger) []accounting.CallRow {
	t.Helper()
	rows, err := l.Calls(time.Time{})
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	return rows
}

// TestSummariseBooksServedFallbackTuple proves a summary a fallback hop
// served (through its own client) books its ledger row on the SERVING tuple:
// Provider and Model name the hop's format and canonical model. Not parallel:
// it makes a ledger the live one.
func TestSummariseBooksServedFallbackTuple(t *testing.T) {
	l := openSummaryLedger(t)
	primary := &fallbackProbeClient{responses: []fallbackProbeResponse{
		{err: &provider.APIError{StatusCode: 503, Body: "unavailable"}},
	}}
	fallback := &fallbackProbeClient{responses: []fallbackProbeResponse{
		{text: "summary from the fallback"},
	}}
	gr := testGroupResolver("anthropic/claude-haiku-4-5")
	s := NewAPISummariser(primary, probeClientProvider{clients: map[string]provider.Client{
		"openai/openai": fallback,
	}}, gr, openAIFallbackFrom("anthropic/claude-haiku-4-5"), func() int { return 0 })

	if _, err := s.Summarise(WithSessionKey(context.Background(), "gil/cfs"), []byte("content"), "sum it", "f.go"); err != nil {
		t.Fatalf("Summarise: %v", err)
	}

	rows := summaryCalls(t, l)
	if len(rows) != 1 {
		t.Fatalf("ledger calls = %d, want the one summary call", len(rows))
	}
	got := rows[0]
	if got.Kind != accounting.KindSummary {
		t.Errorf("call kind = %q, want %q", got.Kind, accounting.KindSummary)
	}
	if got.Provider != "openai" || got.Model != "openai/gpt-5.6" {
		t.Errorf("booked Provider/Model = %q/%q, want openai / openai/gpt-5.6", got.Provider, got.Model)
	}
}

// TestSummariseReusedFallbackAppliesAnthropicDefault proves the
// reused-client rule plus the format default: an ungrouped resolver yields an
// empty primary model/format, and the fallback hop reuses the caller's client
// (nil provider), so the row books the HOP model with the "anthropic" default
// applied to the (empty) served format. Not parallel: it makes a ledger the
// live one.
func TestSummariseReusedFallbackAppliesAnthropicDefault(t *testing.T) {
	l := openSummaryLedger(t)
	shared := &fallbackProbeClient{responses: []fallbackProbeResponse{
		{err: &provider.APIError{StatusCode: 503, Body: "unavailable"}},
		{text: "summary on the caller's client"},
	}}
	// Empty groups: ResolveCall returns nil, so resolveForCall keeps the
	// default client and an empty model/format.
	gr := config.NewGroupResolver(config.GroupsConfig{}, nil, true)
	s := NewAPISummariser(shared, nil, gr, openAIFallbackFrom(""), func() int { return 0 })

	if _, err := s.Summarise(WithSessionKey(context.Background(), "gil/cfs2"), []byte("content"), "sum it", "f.go"); err != nil {
		t.Fatalf("Summarise: %v", err)
	}

	rows := summaryCalls(t, l)
	if len(rows) != 1 {
		t.Fatalf("ledger calls = %d, want the one summary call", len(rows))
	}
	got := rows[0]
	if got.Model != "openai/gpt-5.6" {
		t.Errorf("booked Model = %q, want the hop model openai/gpt-5.6", got.Model)
	}
	if got.Provider != "anthropic" {
		t.Errorf("booked Provider = %q, want the anthropic default applied to the empty served format", got.Provider)
	}
}
