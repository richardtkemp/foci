package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"foci/internal/provider"
)

type mockModelLister struct {
	models []provider.ModelInfo
	err    error
}

func (m *mockModelLister) ListModels() ([]provider.ModelInfo, error) {
	return m.models, m.err
}

func TestResolveAnthropicAliases(t *testing.T) {
	t1 := time.Date(2025, 9, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2025, 10, 1, 0, 0, 0, 0, time.UTC)

	mock := &mockModelLister{
		models: []provider.ModelInfo{
			{ID: "claude-haiku-4-5-20250901", CreatedAt: t1},
			{ID: "claude-haiku-4-5-20251001", CreatedAt: t2},
			{ID: "claude-sonnet-4-6-20250514", CreatedAt: t1},
			{ID: "claude-opus-4-6-20250610", CreatedAt: t1},
		},
	}

	aliases := map[string]string{
		"haiku":  "anthropic/claude-haiku-4-5",
		"sonnet": "anthropic/claude-sonnet-4-6",
		"opus":   "anthropic/claude-opus-4-6",
		"gemini-flash": "google/gemini-2.5-flash",
	}

	resolveAnthropicAliases(mock, aliases)

	// Should resolve to latest dated version
	if got := aliases["haiku"]; got != "anthropic/claude-haiku-4-5-20251001" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5-20251001", got)
	}
	if got := aliases["sonnet"]; got != "anthropic/claude-sonnet-4-6-20250514" {
		t.Errorf("sonnet = %q, want anthropic/claude-sonnet-4-6-20250514", got)
	}
	if got := aliases["opus"]; got != "anthropic/claude-opus-4-6-20250610" {
		t.Errorf("opus = %q, want anthropic/claude-opus-4-6-20250610", got)
	}

	// Non-Anthropic aliases should be untouched
	if got := aliases["gemini-flash"]; got != "google/gemini-2.5-flash" {
		t.Errorf("gemini-flash = %q, want google/gemini-2.5-flash", got)
	}
}

func TestResolveAnthropicAliasesAPIError(t *testing.T) {
	mock := &mockModelLister{
		err: errors.New("connection refused"),
	}

	aliases := map[string]string{
		"haiku":  "anthropic/claude-haiku-4-5",
		"sonnet": "anthropic/claude-sonnet-4-6",
	}

	resolveAnthropicAliases(mock, aliases)

	// Should keep defaults on error
	if got := aliases["haiku"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5 (unchanged)", got)
	}
	if got := aliases["sonnet"]; got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("sonnet = %q, want anthropic/claude-sonnet-4-6 (unchanged)", got)
	}
}

func TestResolveAnthropicAliasesNilMap(t *testing.T) {
	mock := &mockModelLister{}
	// Should not panic
	resolveAnthropicAliases(mock, nil)
}

func TestResolveAnthropicAliasesNoMatchingModels(t *testing.T) {
	mock := &mockModelLister{
		models: []provider.ModelInfo{
			{ID: "claude-sonnet-4-6-20250514", CreatedAt: time.Now()},
		},
	}

	aliases := map[string]string{
		"haiku":  "anthropic/claude-haiku-4-5",
		"sonnet": "anthropic/claude-sonnet-4-6",
	}

	resolveAnthropicAliases(mock, aliases)

	// haiku has no match — should keep default
	if got := aliases["haiku"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5 (unchanged)", got)
	}
	// sonnet should be resolved
	if got := aliases["sonnet"]; got == "anthropic/claude-sonnet-4-6" {
		t.Errorf("sonnet should have been resolved, still %q", got)
	}
}

func TestResolveAnthropicAliasesNoAnthropicAliases(t *testing.T) {
	mock := &mockModelLister{
		models: []provider.ModelInfo{
			{ID: "claude-haiku-4-5-20251001", CreatedAt: time.Now()},
		},
	}

	aliases := map[string]string{
		"gemini-flash": "google/gemini-2.5-flash",
	}

	resolveAnthropicAliases(mock, aliases)

	// Should not call API or modify anything (no anthropic aliases present)
	if got := aliases["gemini-flash"]; got != "google/gemini-2.5-flash" {
		t.Errorf("gemini-flash = %q, want google/gemini-2.5-flash", got)
	}
}

// --- OpenAI alias resolution tests ---

type mockOpenAIModelLister struct {
	models []provider.ModelInfo
	err    error
}

func (m *mockOpenAIModelLister) ListModels(ctx context.Context) ([]provider.ModelInfo, error) {
	return m.models, m.err
}

func TestResolveOpenAIAliases(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
			{ID: "gpt-4o-2025-08-01", CreatedAt: time.Unix(1722470400, 0)},
			{ID: "o3-2025-07-15", CreatedAt: time.Unix(1720000000, 0)},
			{ID: "o4-mini-2025-05-01", CreatedAt: time.Unix(1714521600, 0)},
			{ID: "o4-mini-2025-09-01", CreatedAt: time.Unix(1725148800, 0)},
		},
	}

	aliases := map[string]string{
		"gpt4o":  "openai/gpt-4o",
		"o3":     "openai/o3",
		"o4mini": "openai/o4-mini",
		"haiku":  "anthropic/claude-haiku-4-5",
	}

	resolveOpenAIAliases(context.Background(), mock, aliases)

	if got := aliases["gpt4o"]; got != "openai/gpt-4o-2025-08-01" {
		t.Errorf("gpt4o = %q, want openai/gpt-4o-2025-08-01", got)
	}
	if got := aliases["o3"]; got != "openai/o3-2025-07-15" {
		t.Errorf("o3 = %q, want openai/o3-2025-07-15", got)
	}
	if got := aliases["o4mini"]; got != "openai/o4-mini-2025-09-01" {
		t.Errorf("o4mini = %q, want openai/o4-mini-2025-09-01", got)
	}

	// Non-OpenAI aliases should be untouched
	if got := aliases["haiku"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5", got)
	}
}

func TestResolveOpenAIAliasesAPIError(t *testing.T) {
	mock := &mockOpenAIModelLister{
		err: errors.New("connection refused"),
	}

	aliases := map[string]string{
		"gpt4o": "openai/gpt-4o",
		"o3":    "openai/o3",
	}

	resolveOpenAIAliases(context.Background(), mock, aliases)

	// Should keep defaults on error
	if got := aliases["gpt4o"]; got != "openai/gpt-4o" {
		t.Errorf("gpt4o = %q, want openai/gpt-4o (unchanged)", got)
	}
	if got := aliases["o3"]; got != "openai/o3" {
		t.Errorf("o3 = %q, want openai/o3 (unchanged)", got)
	}
}

func TestResolveOpenAIAliasesNilMap(t *testing.T) {
	mock := &mockOpenAIModelLister{}
	// Should not panic
	resolveOpenAIAliases(context.Background(), mock, nil)
}

func TestResolveOpenAIAliasesNoMatchingModels(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
		},
	}

	aliases := map[string]string{
		"gpt4o":  "openai/gpt-4o",
		"o4mini": "openai/o4-mini",
	}

	resolveOpenAIAliases(context.Background(), mock, aliases)

	// gpt4o should be resolved
	if got := aliases["gpt4o"]; got != "openai/gpt-4o-2025-06-01" {
		t.Errorf("gpt4o = %q, want openai/gpt-4o-2025-06-01", got)
	}
	// o4mini has no match — should keep default
	if got := aliases["o4mini"]; got != "openai/o4-mini" {
		t.Errorf("o4mini = %q, want openai/o4-mini (unchanged)", got)
	}
}

func TestResolveOpenAIAliasesNoOpenAIAliases(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
		},
	}

	aliases := map[string]string{
		"haiku": "anthropic/claude-haiku-4-5",
		"gemini-flash": "google/gemini-2.5-flash",
	}

	resolveOpenAIAliases(context.Background(), mock, aliases)

	// Should not modify anything (no openai aliases present)
	if got := aliases["haiku"]; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5", got)
	}
	if got := aliases["gemini-flash"]; got != "google/gemini-2.5-flash" {
		t.Errorf("gemini-flash = %q, want google/gemini-2.5-flash", got)
	}
}

func TestResolveOpenAIAliasesSkipsNonOpenAIPrefixed(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
		},
	}

	// gpt4o alias points to a non-openai endpoint (e.g. openrouter) — should not be resolved
	aliases := map[string]string{
		"gpt4o": "openrouter/gpt-4o",
	}

	resolveOpenAIAliases(context.Background(), mock, aliases)

	if got := aliases["gpt4o"]; got != "openrouter/gpt-4o" {
		t.Errorf("gpt4o = %q, want openrouter/gpt-4o (unchanged)", got)
	}
}
