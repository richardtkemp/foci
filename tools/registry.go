package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"foci/provider"
)

// ToolResult is the return value from a tool execution.
type ToolResult struct {
	Text        string                   // primary text result (goes into tool_result content)
	ExtraBlocks []provider.ContentBlock // additional content blocks (e.g. document) placed alongside tool_result
}

// TextResult creates a ToolResult with only text (no extra blocks).
func TextResult(s string) ToolResult { return ToolResult{Text: s} }

// Tool is a callable tool with a JSON Schema for parameters.
type Tool struct {
	Name        string
	Description string
	Parameters  json.RawMessage
	Execute     func(ctx context.Context, params json.RawMessage) (ToolResult, error)
	ExecExport  bool // expose as shell function inside exec calls via ExecBridge
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

// ExportedNames returns sorted names of tools with ExecExport: true,
// prefixed with "foci_" (matching the shell function names).
func (r *Registry) ExportedNames() []string {
	var names []string
	for _, t := range r.All() {
		if t.ExecExport {
			names = append(names, "foci_"+t.Name)
		}
	}
	return names
}

// FinalizeExecDescription updates the exec tool's description to include
// the current list of ExecExport shell functions. Call this after all tools
// have been registered so the description stays in sync.
func (r *Registry) FinalizeExecDescription() {
	execTool := r.Get("shell")
	if execTool == nil {
		return
	}
	names := r.ExportedNames()
	if len(names) == 0 {
		return
	}
	suffix := " Shell functions are available for piping tool results within a single command: " + strings.Join(names, ", ") + "."
	if !strings.Contains(execTool.Description, "Shell functions are available") {
		execTool.Description += suffix
	}
}

// ToolDefs returns tool definitions for the Anthropic API, sorted by name
// for deterministic ordering (required for prompt caching — tools are part
// of the cached prefix).
func (r *Registry) ToolDefs() []provider.ToolDef {
	defs := make([]provider.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name() < defs[j].Name() })
	return defs
}
