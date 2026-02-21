package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type mockEscalator struct {
	model string
}

func (m *mockEscalator) SetOverrideModel(model string) {
	m.model = model
}

func TestRequestModelShortName(t *testing.T) {
	esc := &mockEscalator{}
	tool := NewRequestModelTool(esc)

	params, _ := json.Marshal(map[string]string{
		"model":  "opus",
		"reason": "complex reasoning needed",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if esc.model != "claude-opus-4-6" {
		t.Errorf("override model = %q, want claude-opus-4-6", esc.model)
	}
	if !strings.Contains(result, "claude-opus-4-6") {
		t.Errorf("result = %q, expected mention of full model ID", result)
	}
}

func TestRequestModelFullID(t *testing.T) {
	esc := &mockEscalator{}
	tool := NewRequestModelTool(esc)

	params, _ := json.Marshal(map[string]string{
		"model":  "claude-sonnet-4-5",
		"reason": "need better analysis",
	})

	result, err := tool.Execute(context.Background(), params)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if esc.model != "claude-sonnet-4-5" {
		t.Errorf("override model = %q", esc.model)
	}
	if !strings.Contains(result, "claude-sonnet-4-5") {
		t.Errorf("result = %q", result)
	}
}

func TestRequestModelHaiku(t *testing.T) {
	esc := &mockEscalator{}
	tool := NewRequestModelTool(esc)

	params, _ := json.Marshal(map[string]string{
		"model":  "haiku",
		"reason": "simple task",
	})

	tool.Execute(context.Background(), params)

	if esc.model != "claude-haiku-4-5" {
		t.Errorf("override model = %q, want claude-haiku-4-5", esc.model)
	}
}
