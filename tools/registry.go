package tools

import (
	"context"
	"encoding/json"

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

// All returns all registered tools.
func (r *Registry) All() []*Tool {
	out := make([]*Tool, 0, len(r.tools))
	for _, t := range r.tools {
		out = append(out, t)
	}
	return out
}

// ToolDefs returns tool definitions for the Anthropic API.
func (r *Registry) ToolDefs() []anthropic.ToolDef {
	defs := make([]anthropic.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, anthropic.ToolDef{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return defs
}
