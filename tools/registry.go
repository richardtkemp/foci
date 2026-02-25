package tools

import (
	"context"
	"encoding/json"
	"sort"

	"clod/anthropic"
)

// Tool is a callable tool with a JSON Schema for parameters.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Execute     func(ctx context.Context, params json.RawMessage) (string, error)
}

// Registry holds all registered tools.
type Registry struct {
	tools map[string]*Tool
}

// NewRegistry creates an empty tool registry.
func NewRegistry() *Registry {
	return &Registry{tools: make(map[string]*Tool)}
}

// Register adds a tool to the registry.
func (r *Registry) Register(t *Tool) {
	r.tools[t.Name] = t
}

// Get returns a tool by name, or nil if not found.
func (r *Registry) Get(name string) *Tool {
	return r.tools[name]
}

// All returns all registered tools, sorted by name.
func (r *Registry) All() []*Tool {
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ToolDefs returns tool definitions for the Anthropic API, sorted by name
// for deterministic ordering (required for prompt caching — tools are part
// of the cached prefix).
func (r *Registry) ToolDefs() []anthropic.ToolDef {
	defs := make([]anthropic.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, anthropic.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: ensureAdditionalPropertiesFalse(t.Parameters),
			Strict:      true,
		})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// ensureAdditionalPropertiesFalse injects "additionalProperties": false into
// the root object of a JSON schema. Required when strict mode is enabled —
// the API rejects object schemas without this field.
func ensureAdditionalPropertiesFalse(schema json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(schema, &obj); err != nil {
		return schema
	}
	if _, exists := obj["additionalProperties"]; exists {
		return schema // already set
	}
	obj["additionalProperties"] = json.RawMessage("false")
	out, err := json.Marshal(obj)
	if err != nil {
		return schema
	}
	return out
}
