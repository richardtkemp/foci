package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/secrets"
)

func NewReadTool(store *secrets.Store) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return readFile(ctx, params, store, "")
		},
	}
}

func NewWriteTool(store *secrets.Store) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return writeFile(ctx, params, store, "")
		},
	}
}

func NewEditTool(store *secrets.Store) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return editFile(ctx, params, store, "")
		},
	}
}

func NewIsolatedReadTool(store *secrets.Store, baseDir string) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return readFile(ctx, params, store, baseDir)
		},
	}
}

func NewIsolatedWriteTool(store *secrets.Store, baseDir string) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return writeFile(ctx, params, store, baseDir)
		},
	}
}

func NewIsolatedEditTool(store *secrets.Store, baseDir string) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (string, error) {
			return editFile(ctx, params, store, baseDir)
		},
	}
}

func resolveAndValidatePath(path, baseDir string) (string, error) {
	if baseDir == "" {
		return path, nil
	}

	evalBase, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return "", fmt.Errorf("resolve base dir: %w", err)
	}

	if filepath.IsAbs(path) {
		return "", fmt.Errorf("absolute paths not allowed in isolated mode")
	}

	resolved := filepath.Clean(filepath.Join(baseDir, path))

	evalResolved, err := filepath.EvalSymlinks(resolved)
	if err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	if err == nil {
		resolved = evalResolved
	} else {
		// File doesn't exist yet — resolve the deepest existing ancestor
		// to catch symlinks on intermediate path components.
		dir := resolved
		var tail []string
		for {
			parent := filepath.Dir(dir)
			tail = append(tail, filepath.Base(dir))
			dir = parent
			evalDir, err := filepath.EvalSymlinks(dir)
			if err == nil {
				// Rebuild: resolved ancestor + unresolved tail components
				for i := len(tail) - 1; i >= 0; i-- {
					evalDir = filepath.Join(evalDir, tail[i])
				}
				resolved = evalDir
				break
			}
			if !os.IsNotExist(err) {
				return "", fmt.Errorf("resolve ancestor: %w", err)
			}
		}
	}

	if !strings.HasPrefix(resolved, evalBase+string(filepath.Separator)) && resolved != evalBase {
		return "", fmt.Errorf("path traversal outside isolated directory not allowed")
	}

	return resolved, nil
}

// checkBlockedPath resolves the path to absolute and checks against blocked paths.
func checkBlockedPath(store *secrets.Store, path string) error {
	if store == nil {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if store.IsBlockedPath(abs) {
		return fmt.Errorf("access denied: path is restricted")
	}
	return nil
}

func readFile(ctx context.Context, params json.RawMessage, store *secrets.Store, baseDir string) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	resolved, err := resolveAndValidatePath(p.Path, baseDir)
	if err != nil {
		return "", err
	}

	if err := checkBlockedPath(store, resolved); err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolved)
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

func writeFile(ctx context.Context, params json.RawMessage, store *secrets.Store, baseDir string) (string, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	resolved, err := resolveAndValidatePath(p.Path, baseDir)
	if err != nil {
		return "", err
	}

	if err := checkBlockedPath(store, resolved); err != nil {
		return "", err
	}

	if err := os.WriteFile(resolved, []byte(p.Content), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	return fmt.Sprintf("Wrote %d bytes to %s", len(p.Content), p.Path), nil
}

func editFile(ctx context.Context, params json.RawMessage, store *secrets.Store, baseDir string) (string, error) {
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return "", fmt.Errorf("parse params: %w", err)
	}

	resolved, err := resolveAndValidatePath(p.Path, baseDir)
	if err != nil {
		return "", err
	}

	if err := checkBlockedPath(store, resolved); err != nil {
		return "", err
	}

	data, err := os.ReadFile(resolved)
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

	preErr := checkSyntax(resolved, data)
	newContent := strings.Replace(content, p.OldString, p.NewString, 1)

	if preErr == nil {
		if postErr := checkSyntax(resolved, []byte(newContent)); postErr != nil {
			return "", fmt.Errorf("edit rejected — would introduce syntax error: %v", postErr)
		}
	}

	if err := os.WriteFile(resolved, []byte(newContent), 0644); err != nil {
		return "", fmt.Errorf("write file: %w", err)
	}

	msg := fmt.Sprintf("Edited %s", p.Path)
	if preErr != nil {
		msg += fmt.Sprintf("\nWarning: file already had syntax errors: %v", preErr)
	}
	return msg, nil
}
