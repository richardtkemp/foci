package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"foci/internal/secrets"
)

// NewSummaryTool creates a tool that summarises or extracts information from
// a file (or piped stdin) via a fast, cheap model call without loading the
// full content into the agent's context.
//
// Inference dispatch is decoupled from the tool body: the caller passes a
// Summariser implementation chosen at agent setup time. API-mode agents get
// APISummariser (provider.Send through foci's client). Delegated/CC-mode
// agents get CLISummariser (shells out to `claude --print` using the parent
// CC subprocess's subscription auth). Adding a third backend later is a new
// Summariser impl, not a tool rewrite.
//
// Stdin handling lives in the bash wrapper (execbridge.go's "summary" case):
// piped input is written to a temp file and passed via the file param. So
// `cat foo.txt | foci_summary "summarise this"` works without the Go-side
// tool needing stdin awareness.
func NewSummaryTool(store *secrets.Store, summariser Summariser, workspace string) *Tool {
	return &Tool{
		Name:        "summary",
		Description: "Summarize or extract specific information from a file using a fast, cheap model call. Do NOT use this to read or dump full file contents — use the read tool for that. This tool is for targeted questions like 'what config options are defined?' or 'summarize the error handling approach', not for retrieving the file text itself.",
		ExecExport:  true,
		Positional:  []string{"prompt"},
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"file": {
					"type": "string",
					"description": "Path to the file to summarize"
				},
				"prompt": {
					"type": "string",
					"description": "What to extract or summarize from the file"
				}
			},
			"required": ["file", "prompt"]
		}`),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return summaryExecute(ctx, params, fileScope{store: store, workspace: workspace}, summariser)
		},
	}
}

// NewIsolatedSummaryTool wraps a summary tool for raw/isolated spawns, confining
// its file argument to baseDir (plus the blocklist) so a sandboxed spawn cannot
// summarise — and thereby exfiltrate — files outside its temp dir.
func NewIsolatedSummaryTool(base *Tool, store *secrets.Store, baseDir string) *Tool {
	fs := fileScope{store: store, baseDir: baseDir}
	return &Tool{
		Name:        base.Name,
		Description: base.Description,
		Parameters:  base.Parameters,
		Execute: func(ctx context.Context, input json.RawMessage) (ToolResult, error) {
			contained, err := rewriteContainedPaths(input, fs, []string{"file"}, "", "")
			if err != nil {
				return ToolResult{}, err
			}
			return base.Execute(ctx, contained)
		},
	}
}

func summaryExecute(ctx context.Context, params json.RawMessage, fs fileScope, summariser Summariser) (ToolResult, error) {
	p, err := UnmarshalParams[struct {
		File   string `json:"file"`
		Prompt string `json:"prompt"`
	}](params)
	if err != nil {
		return ToolResult{}, err
	}

	if p.File == "" {
		return ToolResult{}, fmt.Errorf("file parameter is required")
	}
	if p.Prompt == "" {
		return ToolResult{}, fmt.Errorf("prompt parameter is required")
	}

	filePath, err := fs.resolveFileArg(p.File)
	if err != nil {
		return ToolResult{}, err
	}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read file: %w", err)
	}

	if len(data) == 0 {
		return ToolResult{}, fmt.Errorf("file is empty: %s", p.File)
	}

	// Detect binary files by checking for null bytes in first 512 bytes.
	checkLen := len(data)
	if checkLen > 512 {
		checkLen = 512
	}
	for _, b := range data[:checkLen] {
		if b == 0 {
			return ToolResult{}, fmt.Errorf("file appears to be binary: %s", p.File)
		}
	}

	text, err := summariser.Summarise(ctx, data, p.Prompt, p.File)
	if err != nil {
		return ToolResult{}, err
	}
	return TextResult(text), nil
}
