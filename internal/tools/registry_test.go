package tools

import (
	"context"
	"encoding/json"
	"testing"
)

func TestRegistryRegisterAndGet(t *testing.T) {
	t.Parallel()
	r := NewRegistry()

	tool := &Tool{
		Name:        "test_tool",
		Description: "a test",
		Parameters:  json.RawMessage(`{"type":"object"}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return TextResult("ok"), nil
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
	t.Parallel()
	r := NewRegistry()
	if got := r.Get("nonexistent"); got != nil {
		t.Errorf("Get(nonexistent) = %v, want nil", got)
	}
}

func TestRegistryAll(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
	r := NewRegistry()
	r.Register(&Tool{
		Name:        "shell",
		Description: "run commands",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`),
	})

	defs := r.ToolDefs()
	if len(defs) != 1 {
		t.Fatalf("len = %d, want 1", len(defs))
	}
	if defs[0].Name() != "shell" {
		t.Errorf("Name = %q", defs[0].Name())
	}

	// ToolDef should round-trip via JSON and contain expected fields
	data, err := json.Marshal(defs[0])
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]interface{}
	json.Unmarshal(data, &raw)
	if raw["description"] != "run commands" {
		t.Errorf("description = %v", raw["description"])
	}
	if raw["input_schema"] == nil {
		t.Error("input_schema missing from serialized ToolDef")
	}
}

func TestRegistryOverwrite(t *testing.T) {
	t.Parallel()
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
