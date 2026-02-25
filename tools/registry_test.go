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

	// InputSchema should be valid JSON with additionalProperties injected
	var schema map[string]interface{}
	if err := json.Unmarshal(defs[0].InputSchema, &schema); err != nil {
		t.Errorf("InputSchema not valid JSON: %v", err)
	}
	if ap, ok := schema["additionalProperties"]; !ok {
		t.Error("additionalProperties not injected into schema")
	} else if ap != false {
		t.Errorf("additionalProperties = %v, want false", ap)
	}
	if !defs[0].Strict {
		t.Error("Strict should be true")
	}
}

func TestStrictifySchema(t *testing.T) {
	tests := []struct {
		name  string
		input string
		check func(t *testing.T, obj map[string]interface{})
	}{
		{
			"injects at root",
			`{"type":"object","properties":{"a":{"type":"string"}}}`,
			func(t *testing.T, obj map[string]interface{}) {
				if obj["additionalProperties"] != false {
					t.Error("root additionalProperties should be false")
				}
			},
		},
		{
			"preserves existing additionalProperties",
			`{"type":"object","additionalProperties":{"type":"string"}}`,
			func(t *testing.T, obj map[string]interface{}) {
				// No properties key, so should not be touched
				if _, ok := obj["additionalProperties"].(map[string]interface{}); !ok {
					t.Error("should preserve existing additionalProperties object")
				}
			},
		},
		{
			"recurses into nested objects",
			`{"type":"object","properties":{"nested":{"type":"object","properties":{"x":{"type":"string"}}}}}`,
			func(t *testing.T, obj map[string]interface{}) {
				if obj["additionalProperties"] != false {
					t.Error("root additionalProperties should be false")
				}
				props := obj["properties"].(map[string]interface{})
				nested := props["nested"].(map[string]interface{})
				if nested["additionalProperties"] != false {
					t.Error("nested additionalProperties should be false")
				}
			},
		},
		{
			"recurses into array items",
			`{"type":"object","properties":{"list":{"type":"array","items":{"type":"object","properties":{"v":{"type":"string"}}}}}}`,
			func(t *testing.T, obj map[string]interface{}) {
				props := obj["properties"].(map[string]interface{})
				list := props["list"].(map[string]interface{})
				items := list["items"].(map[string]interface{})
				if items["additionalProperties"] != false {
					t.Error("array items additionalProperties should be false")
				}
			},
		},
		{
			"invalid JSON unchanged",
			`not json`,
			func(t *testing.T, obj map[string]interface{}) {
				// Should not panic; obj will be nil
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := strictifySchema(json.RawMessage(tt.input))
			var obj map[string]interface{}
			if err := json.Unmarshal(out, &obj); err != nil {
				if tt.name == "invalid JSON unchanged" {
					return // expected
				}
				t.Fatalf("result not valid JSON: %v", err)
			}
			tt.check(t, obj)
		})
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
