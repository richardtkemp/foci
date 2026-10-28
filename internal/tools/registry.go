package tools

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"foci/internal/provider"
)

// ToolResult is the return value from a tool execution.
type ToolResult struct {
	Text        string                  // primary text result (goes into tool_result content)
	ExtraBlocks []provider.ContentBlock // additional content blocks (e.g. document) placed alongside tool_result
	ResultFile  string                  // if set, full result is already at this path (skip redundant write in guard)
	ResultSize  int64                   // total bytes of the full result (0 if not spilled)
	// JSON marks Text as already machine-readable (a JSON document, or todo's
	// JSONL) for a --json caller (#1215). When a --json call returns a result
	// without it, the exec bridge wraps Text as {"result": ...} instead.
	JSON bool
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

	// Positional declares schema parameters that appear as bare args in
	// the generated shell function instead of --flag form. Multi-word
	// values are space-joined to match existing UX (`foci_web_search the
	// quick brown fox` → `query="the quick brown fox"`). Currently
	// single-element only. Used by the schema-driven generic generator.
	Positional []string

	// StdinParam declares which schema parameter consumes stdin when no
	// explicit value is provided and stdin is not a TTY. Used by tools
	// that accept piped content (send_to_chat, summary).
	StdinParam string

	// Aliases maps a canonical schema parameter name to one or more
	// alternative flag names. The generated shell function emits a case
	// arm for each alias that assigns into the canonical variable, so
	// `--description X` and `--text X` populate the same field.
	//
	// Aliases use snake_case names; the generator kebab-cases them for
	// the actual flag (e.g. "send_as" → --send-as). Values that don't
	// reference a real schema property are silently ignored.
	Aliases map[string][]string

	// JSONOutput documents the shape a --json shell call prints (#1215),
	// shown under "Output (--json):" in --help. Set it on a tool whose
	// Execute renders its own JSON (via JSONResult) when WantsJSON(ctx);
	// empty means the exec bridge's generic {"result": "<text>"} wrapper.
	JSONOutput string

	// Available is a live gate (#2603, a config switch such as [depth]
	// enabled); nil = always available. While it reports false the tool is
	// left out of what the model is shown (ToolDefs, ExportedTools, the shell
	// tool's description), but stays registered: its shell function and
	// auto-approve rule exist, so a running session gains it the moment the
	// gate opens. Execute must therefore refuse by itself while closed.
	Available func() bool
}

// available reports whether t is open to the model right now.
func (t *Tool) available() bool { return t.Available == nil || t.Available() }

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

// ExportedTool holds the shell function name and description for an exported tool.
type ExportedTool struct {
	Name        string // e.g. "foci_todo"
	Description string // first sentence of the tool's description
}

// ExportedNames returns sorted names of tools with ExecExport: true,
// prefixed with "foci_" (matching the shell function names). It includes
// live-gated tools whatever their gate (Tool.Available): it drives the
// auto-approve rules, built once at startup.
func (r *Registry) ExportedNames() []string {
	var names []string
	for _, t := range r.All() {
		if t.ExecExport {
			names = append(names, "foci_"+t.Name)
		}
	}
	return names
}

// ExportedTools returns sorted exported tools with names and descriptions,
// for the listing the model reads: a tool whose Available gate is closed is
// left out.
func (r *Registry) ExportedTools() []ExportedTool {
	var out []ExportedTool
	for _, t := range r.All() {
		if t.ExecExport && t.available() {
			out = append(out, ExportedTool{
				Name:        "foci_" + t.Name,
				Description: firstSentence(t.Description),
			})
		}
	}
	return out
}

// firstSentence returns the text up to the first period followed by a space,
// or the full string if no such boundary exists.
func firstSentence(s string) string {
	for i := 0; i < len(s)-1; i++ {
		if s[i] == '.' && s[i+1] == ' ' {
			return s[:i+1]
		}
	}
	return s
}

// FinalizeShellDescription updates the shell tool's description to include
// the current list of ExecExport shell functions. Call this after all tools
// have been registered so the description stays in sync.
func (r *Registry) FinalizeShellDescription() {
	execTool := r.Get("shell")
	if execTool == nil {
		return
	}
	// Written once at startup, so a live-gated tool is left out: the model
	// learns of it through ToolDefs, which follows the gate.
	var names []string
	for _, t := range r.All() {
		if t.ExecExport && t.Available == nil {
			names = append(names, "foci_"+t.Name)
		}
	}
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
// of the cached prefix). A tool whose Available gate is closed is left out.
func (r *Registry) ToolDefs() []provider.ToolDef {
	defs := make([]provider.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		if !t.available() {
			continue
		}
		defs = append(defs, provider.NewCustomTool(t.Name, t.Description, t.Parameters))
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name() < defs[j].Name() })
	return defs
}
