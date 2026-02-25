package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	r := NewRegistry()

	tool := &Tool{
		Name:        "test_tool",
		Description: "a test",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return "ok", nil
		},
	}

	r.Register(tool)

	got := r.Get("test_tool")
	if got == nil {
		t.Fatal("Get returned nil")
	}
	if got.Name != "test_tool" {
		t.Errorf("Name = %q", got.Name)
	}
}

func TestRegistryGetMissing(t *testing.T) {
	r := NewRegistry()
	if got := r.Get("nonexistent"); got != nil {
		t.Errorf("Get(nonexistent) = %v, want nil", got)
	}
}

func TestRegistryAll(t *testing.T) {
	r := NewRegistry()
	r.Register(&Tool{Name: "a"})
	r.Register(&Tool{Name: "b"})
	r.Register(&Tool{Name: "c"})

	all := r.All()
	if len(all) != 3 {
		t.Fatalf("len(All) = %d, want 3", len(all))
	}

	names := map[string]bool{}
	for _, t := range all {
		names[t.Name] = true
	}
	for _, name := range []string{"a", "b", "c"} {
		if !names[name] {
			t.Errorf("missing tool %q", name)
		}
	}
}

func TestRegistryToolDefs(t *testing.T) {
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "exec",
		Description: "run commands",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`),
	})

	defs := r.ToolDefs()
	if len(defs) != 1 {
		t.Fatalf("len = %d, want 1", len(defs))
	}
	if defs[0].Name != "exec" {
		t.Errorf("Name = %q", defs[0].Name)
	}
	if defs[0].Description != "run commands" {
		t.Errorf("Description = %q", defs[0].Description)
	}

	// InputSchema should be valid JSON
	var schema map[string]interface{}
	if err := json.Unmarshal(defs[0].InputSchema, &schema); err != nil {
		t.Errorf("InputSchema not valid JSON: %v", err)
	}
}

func TestRegistryOverwrite(t *testing.T) {
	r := NewRegistry()
	r.Register(&Tool{Name: "x", Description: "first"})
	r.Register(&Tool{Name: "x", Description: "second"})

	got := r.Get("x")
	if got.Description != "second" {
		t.Errorf("Description = %q, want %q", got.Description, "second")
	}

	if len(r.All()) != 1 {
		t.Errorf("len(All) = %d after overwrite, want 1", len(r.All()))
	}
}
