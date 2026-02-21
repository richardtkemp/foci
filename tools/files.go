package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func NewReadTool() *Tool {
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file. Returns file contents with line numbers.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file to read"
				}
			},
			"required": ["path"]
		}`),
		Execute: readFile,
	}
}

func NewWriteTool() *Tool {
	return &Tool{
		Name:        "write",
		Description: "Create or overwrite a file with the given content.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file to write"
				},
				"content": {
					"type": "string",
					"description": "Content to write to the file"
				}
			},
			"required": ["path", "content"]
		}`),
		Execute: writeFile,
	}
}

func NewEditTool() *Tool {
	return &Tool{
		Name:        "edit",
		Description: "Find and replace text in a file. The old_string must appear exactly once in the file.",
		Parameters: json.RawMessage(`{
			"type": "object",
			"properties": {
				"path": {
					"type": "string",
					"description": "Path to the file to edit"
				},
				"old_string": {
					"type": "string",
					"description": "Text to find (must be unique in file)"
				},
				"new_string": {
					"type": "string",
					"description": "Replacement text"
				}
			},
			"required": ["path", "old_string", "new_string"]
		}`),
		Execute: editFile,
	}
}

func readFile(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	content := string(data)
	lines := strings.Split(content, "\n")

	const maxLines = 2000
	if len(lines) > maxLines {
		lines = lines[:maxLines]
		lines = append(lines, fmt.Sprintf("... (%d lines truncated)", len(strings.Split(content, "\n"))-maxLines))
	}

	var out strings.Builder
	for i, line := range lines {
		fmt.Fprintf(&out, "%4d\t%s\n", i+1, line)
	}
	return out.String(), nil
}

func writeFile(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	if err := os.WriteFile(p.Path, []byte(p.Content), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return fmt.Sprintf("Wrote %d bytes to %s", len(p.Content), p.Path), nil
}

func editFile(ctx context.Context, params json.RawMessage) (string, error) {
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	data, err := os.ReadFile(p.Path)
	if err != nil {
		return "", fmt.Errorf("read file: %w", err)
	}

	content := string(data)
	count := strings.Count(content, p.OldString)

	if count == 0 {
		return "", fmt.Errorf("old_string not found in %s", p.Path)
	}
	if count > 1 {
		return "", fmt.Errorf("old_string found %d times in %s (must be unique)", count, p.Path)
	}

	newContent := strings.Replace(content, p.OldString, p.NewString, 1)
	if err := os.WriteFile(p.Path, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return fmt.Sprintf("Edited %s", p.Path), nil
}
