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
		Strict:      true,
		Parameters:  json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"}}}`),
	})
	r.Register(&Tool{
		Name:        "simple",
		Description: "simple tool",
		Parameters:  json.RawMessage(`{"type":"object","properties":{"q":{"type":"string"}}}`),
	})

	defs := r.ToolDefs()
	if len(defs) != 2 {
		t.Fatalf("len = %d, want 2", len(defs))
	}

	// exec is strict — should have additionalProperties injected
	exec := defs[0] // sorted: exec < simple
	if exec.Name != "exec" {
		t.Errorf("Name = %q", exec.Name)
	}
	if !exec.Strict {
		t.Error("exec should be strict")
	}
	var schema map[string]interface{}
	if err := json.Unmarshal(exec.InputSchema, &schema); err != nil {
		t.Fatalf("InputSchema not valid JSON: %v", err)
	}
	if ap, ok := schema["additionalProperties"]; !ok {
		t.Error("additionalProperties not injected into strict schema")
	} else if ap != false {
		t.Errorf("additionalProperties = %v, want false", ap)
	}

	// simple is not strict — no additionalProperties injected
	simple := defs[1]
	if simple.Strict {
		t.Error("simple should not be strict")
	}
	var simpleSchema map[string]interface{}
	if err := json.Unmarshal(simple.InputSchema, &simpleSchema); err != nil {
		t.Fatalf("InputSchema not valid JSON: %v", err)
	}
	if _, ok := simpleSchema["additionalProperties"]; ok {
		t.Error("non-strict schema should not have additionalProperties injected")
	}
}

// TestStrictToolLimit ensures the API limit of 20 strict tools is not exceeded.
// If you add a new strict tool, update this list. If the count exceeds 20,
// either remove strict from a less-important tool or wait for Anthropic to
// raise the limit.
func TestStrictToolLimit(t *testing.T) {
	const maxStrict = 20

	// Exhaustive list of tools with Strict: true.
	// If you add Strict to a new tool, add it here.
	strictTools := []string{
		"bitwarden_search",
		"bitwarden_unlock",
		"edit",
		"exec",
		"http_request",
		"memory_remind",
		"schedule_wake",
		"send_telegram",
		"send_to_session",
		"spawn",
		"tmux",
		"todo",
		"write",
	}

	if len(strictTools) > maxStrict {
		t.Errorf("too many strict tools (%d > %d): %v", len(strictTools), maxStrict, strictTools)
	}

	// Verify the list matches reality by checking the non-dep tools we can construct.
	for _, fn := range []struct {
		name string
		tool *Tool
	}{
		{"write", NewWriteTool()},
		{"edit", NewEditTool()},
		{"read", NewReadTool()},
	} {
		wantStrict := false
		for _, s := range strictTools {
			if s == fn.name {
				wantStrict = true
				break
			}
		}
		if fn.tool.Strict != wantStrict {
			t.Errorf("tool %q: Strict=%v, expected %v", fn.name, fn.tool.Strict, wantStrict)
		}
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
