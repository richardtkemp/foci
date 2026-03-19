package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"foci/internal/config"
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

	configs := map[string]config.ModelConfig{
		"haiku":         {Model: "anthropic/claude-haiku-4-5"},
		"sonnet":        {Model: "anthropic/claude-sonnet-4-6"},
		"opus":          {Model: "anthropic/claude-opus-4-6"},
		"gemini-flash":  {Model: "google/gemini-2.5-flash"},
	}

	resolveAnthropicModelConfigs(mock, configs)

	// Should resolve to latest dated version
	if got := configs["haiku"].Model; got != "anthropic/claude-haiku-4-5-20251001" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5-20251001", got)
	}
	if got := configs["sonnet"].Model; got != "anthropic/claude-sonnet-4-6-20250514" {
		t.Errorf("sonnet = %q, want anthropic/claude-sonnet-4-6-20250514", got)
	}
	if got := configs["opus"].Model; got != "anthropic/claude-opus-4-6-20250610" {
		t.Errorf("opus = %q, want anthropic/claude-opus-4-6-20250610", got)
	}

	// Non-Anthropic configs should be untouched
	if got := configs["gemini-flash"].Model; got != "google/gemini-2.5-flash" {
		t.Errorf("gemini-flash = %q, want google/gemini-2.5-flash", got)
	}
}

func TestResolveAnthropicAliasesAPIError(t *testing.T) {
	mock := &mockModelLister{
		err: errors.New("connection refused"),
	}

	configs := map[string]config.ModelConfig{
		"haiku":  {Model: "anthropic/claude-haiku-4-5"},
		"sonnet": {Model: "anthropic/claude-sonnet-4-6"},
	}

	resolveAnthropicModelConfigs(mock, configs)

	// Should keep defaults on error
	if got := configs["haiku"].Model; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5 (unchanged)", got)
	}
	if got := configs["sonnet"].Model; got != "anthropic/claude-sonnet-4-6" {
		t.Errorf("sonnet = %q, want anthropic/claude-sonnet-4-6 (unchanged)", got)
	}
}

func TestResolveAnthropicModelConfigsNilMap(t *testing.T) {
	mock := &mockModelLister{}
	// Should not panic
	resolveAnthropicModelConfigs(mock, nil)
}

func TestResolveAnthropicModelConfigsNoMatchingModels(t *testing.T) {
	mock := &mockModelLister{
		models: []provider.ModelInfo{
			{ID: "claude-sonnet-4-6-20250514", CreatedAt: time.Now()},
		},
	}

	configs := map[string]config.ModelConfig{
		"haiku":  {Model: "anthropic/claude-haiku-4-5"},
		"sonnet": {Model: "anthropic/claude-sonnet-4-6"},
	}

	resolveAnthropicModelConfigs(mock, configs)

	// haiku has no match — should keep default
	if got := configs["haiku"].Model; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5 (unchanged)", got)
	}
	// sonnet should be resolved
	if got := configs["sonnet"].Model; got == "anthropic/claude-sonnet-4-6" {
		t.Errorf("sonnet should have been resolved, still %q", got)
	}
}

func TestResolveAnthropicModelConfigsNoAnthropicConfigs(t *testing.T) {
	mock := &mockModelLister{
		models: []provider.ModelInfo{
			{ID: "claude-haiku-4-5-20251001", CreatedAt: time.Now()},
		},
	}

	configs := map[string]config.ModelConfig{
		"gemini-flash": {Model: "google/gemini-2.5-flash"},
	}

	resolveAnthropicModelConfigs(mock, configs)

	// Should not call API or modify anything (no anthropic configs present)
	if got := configs["gemini-flash"].Model; got != "google/gemini-2.5-flash" {
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

func TestResolveOpenAIModelConfigs(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
			{ID: "gpt-4o-2025-08-01", CreatedAt: time.Unix(1722470400, 0)},
			{ID: "o3-2025-07-15", CreatedAt: time.Unix(1720000000, 0)},
			{ID: "o4-mini-2025-05-01", CreatedAt: time.Unix(1714521600, 0)},
			{ID: "o4-mini-2025-09-01", CreatedAt: time.Unix(1725148800, 0)},
		},
	}

	configs := map[string]config.ModelConfig{
		"gpt4o":  {Model: "openai/gpt-4o"},
		"o3":     {Model: "openai/o3"},
		"o4mini": {Model: "openai/o4-mini"},
		"haiku":  {Model: "anthropic/claude-haiku-4-5"},
	}

	resolveOpenAIModelConfigs(context.Background(), mock, configs)

	if got := configs["gpt4o"].Model; got != "openai/gpt-4o-2025-08-01" {
		t.Errorf("gpt4o = %q, want openai/gpt-4o-2025-08-01", got)
	}
	if got := configs["o3"].Model; got != "openai/o3-2025-07-15" {
		t.Errorf("o3 = %q, want openai/o3-2025-07-15", got)
	}
	if got := configs["o4mini"].Model; got != "openai/o4-mini-2025-09-01" {
		t.Errorf("o4mini = %q, want openai/o4-mini-2025-09-01", got)
	}

	// Non-OpenAI configs should be untouched
	if got := configs["haiku"].Model; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5", got)
	}
}

func TestResolveOpenAIModelConfigsAPIError(t *testing.T) {
	mock := &mockOpenAIModelLister{
		err: errors.New("connection refused"),
	}

	configs := map[string]config.ModelConfig{
		"gpt4o": {Model: "openai/gpt-4o"},
		"o3":    {Model: "openai/o3"},
	}

	resolveOpenAIModelConfigs(context.Background(), mock, configs)

	// Should keep defaults on error
	if got := configs["gpt4o"].Model; got != "openai/gpt-4o" {
		t.Errorf("gpt4o = %q, want openai/gpt-4o (unchanged)", got)
	}
	if got := configs["o3"].Model; got != "openai/o3" {
		t.Errorf("o3 = %q, want openai/o3 (unchanged)", got)
	}
}

func TestResolveOpenAIModelConfigsNilMap(t *testing.T) {
	mock := &mockOpenAIModelLister{}
	// Should not panic
	resolveOpenAIModelConfigs(context.Background(), mock, nil)
}

func TestResolveOpenAIModelConfigsNoMatchingModels(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
		},
	}

	configs := map[string]config.ModelConfig{
		"gpt4o":  {Model: "openai/gpt-4o"},
		"o4mini": {Model: "openai/o4-mini"},
	}

	resolveOpenAIModelConfigs(context.Background(), mock, configs)

	// gpt4o should be resolved
	if got := configs["gpt4o"].Model; got != "openai/gpt-4o-2025-06-01" {
		t.Errorf("gpt4o = %q, want openai/gpt-4o-2025-06-01", got)
	}
	// o4mini has no match — should keep default
	if got := configs["o4mini"].Model; got != "openai/o4-mini" {
		t.Errorf("o4mini = %q, want openai/o4-mini (unchanged)", got)
	}
}

func TestResolveOpenAIModelConfigsNoOpenAIConfigs(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
		},
	}

	configs := map[string]config.ModelConfig{
		"haiku":        {Model: "anthropic/claude-haiku-4-5"},
		"gemini-flash": {Model: "google/gemini-2.5-flash"},
	}

	resolveOpenAIModelConfigs(context.Background(), mock, configs)

	// Should not modify anything (no openai configs present)
	if got := configs["haiku"].Model; got != "anthropic/claude-haiku-4-5" {
		t.Errorf("haiku = %q, want anthropic/claude-haiku-4-5", got)
	}
	if got := configs["gemini-flash"].Model; got != "google/gemini-2.5-flash" {
		t.Errorf("gemini-flash = %q, want google/gemini-2.5-flash", got)
	}
}

func TestResolveOpenAIModelConfigsSkipsNonOpenAIPrefixed(t *testing.T) {
	mock := &mockOpenAIModelLister{
		models: []provider.ModelInfo{
			{ID: "gpt-4o-2025-06-01", CreatedAt: time.Unix(1717200000, 0)},
		},
	}

	// gpt4o config points to a non-openai endpoint (e.g. openrouter) — should not be resolved
	configs := map[string]config.ModelConfig{
		"gpt4o": {Model: "openrouter/gpt-4o"},
	}

	resolveOpenAIModelConfigs(context.Background(), mock, configs)

	if got := configs["gpt4o"].Model; got != "openrouter/gpt-4o" {
		t.Errorf("gpt4o = %q, want openrouter/gpt-4o (unchanged)", got)
	}
}
