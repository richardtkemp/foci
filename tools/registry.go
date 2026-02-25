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
			InputSchema: strictifySchema(t.Parameters),
			Strict:      true,
		})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// strictifySchema recursively walks a JSON schema and injects
// "additionalProperties": false on every object that has "properties".
// Required for strict tool mode — the API rejects schemas where any
// object with properties lacks this field.
func strictifySchema(schema json.RawMessage) json.RawMessage {
	var node interface{}
	if err := json.Unmarshal(schema, &node); err != nil {
		return schema
	}
	strictifyNode(node)
	out, err := json.Marshal(node)
	if err != nil {
		return schema
	}
	return out
}

func strictifyNode(node interface{}) {
	obj, ok := node.(map[string]interface{})
	if !ok {
		return
	}

	// If this object has "properties", it's a schema object — enforce strict.
	if _, hasProps := obj["properties"]; hasProps {
		if _, hasAP := obj["additionalProperties"]; !hasAP {
			obj["additionalProperties"] = false
		}
		// Recurse into each property's schema
		if props, ok := obj["properties"].(map[string]interface{}); ok {
			for _, v := range props {
				strictifyNode(v)
			}
		}
	}

	// Recurse into "items" (array items schema)
	if items, ok := obj["items"]; ok {
		strictifyNode(items)
	}
}
