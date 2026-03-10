package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"foci/internal/provider"
	"foci/internal/config"
	"foci/internal/secrets"
)

const readToolSchema = `{
	"type": "object",
	"properties": {
		"path": {
			"type": "string",
			"description": "Path to the file to read"
		},
		"offset": {
			"type": "integer",
			"description": "Line number to start reading from (1-based, default: 1)"
		},
		"limit": {
			"type": "integer",
			"description": "Maximum number of lines to return (default: 2000)"
		}
	},
	"required": ["path"]
}`

func NewReadTool(store *secrets.Store) *Tool {
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file (line-numbered) or list a directory. Use offset/limit to read a specific range of lines.",
		Parameters: json.RawMessage(readToolSchema),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return readFile(ctx, params, store, "")
		},
	}
}

func NewWriteTool(store *secrets.Store, blockedPaths []config.BlockedPath) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return writeFile(ctx, params, store, "", blockedPaths)
		},
	}
}

func NewEditTool(store *secrets.Store, blockedPaths []config.BlockedPath) *Tool {
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return editFile(ctx, params, store, "", blockedPaths)
		},
	}
}

func NewIsolatedReadTool(store *secrets.Store, baseDir string) *Tool {
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file (line-numbered) or list a directory. Use offset/limit to read a specific range of lines.",
		Parameters: json.RawMessage(readToolSchema),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
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
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
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

// checkConfigBlockedPath checks if the resolved path falls under any configured
// blocked path prefix. Returns (rebuke, true) if blocked. This is a soft nudge,
// not a security boundary — the rebuke is returned as a successful tool result.
func checkConfigBlockedPath(blockedPaths []config.BlockedPath, resolved string) (string, bool) {
	if len(blockedPaths) == 0 {
		return "", false
	}
	abs, err := filepath.Abs(resolved)
	if err != nil {
		abs = resolved
	}
	for _, bp := range blockedPaths {
		prefix, err := filepath.Abs(bp.Path)
		if err != nil {
			prefix = bp.Path
		}
		if abs == prefix || strings.HasPrefix(abs, prefix+string(filepath.Separator)) {
			return bp.Rebuke, true
		}
	}
	return "", false
}

func readFile(ctx context.Context, params json.RawMessage, store *secrets.Store, baseDir string) (ToolResult, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	resolved, err := resolveAndValidatePath(p.Path, baseDir)
	if err != nil {
		return ToolResult{}, err
	}

	if err := checkBlockedPath(store, resolved); err != nil {
		return ToolResult{}, err
	}

	info, err := os.Stat(resolved)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read file: %w", err)
	}
	if info.IsDir() {
		entries, err := os.ReadDir(resolved)
		if err != nil {
			return ToolResult{}, fmt.Errorf("read directory: %w", err)
		}
		var out strings.Builder
		fmt.Fprintf(&out, "Directory listing: %s\n\n", p.Path)
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				name += "/"
			}
			fmt.Fprintf(&out, "  %s\n", name)
		}
		if len(entries) == 0 {
			fmt.Fprintf(&out, "  (empty directory)\n")
		}
		return TextResult(out.String()), nil
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read file: %w", err)
	}

	// PDF files: base64-encode and return as document content block
	if strings.EqualFold(filepath.Ext(resolved), ".pdf") {
		const maxPDFSize = 32 * 1024 * 1024
		if len(data) > maxPDFSize {
			return ToolResult{}, fmt.Errorf("PDF too large: %d bytes (max %d)", len(data), maxPDFSize)
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		return ToolResult{
			Text:        fmt.Sprintf("[PDF: %s, %d bytes]", filepath.Base(resolved), len(data)),
			ExtraBlocks: []provider.ContentBlock{provider.DocumentBlock("application/pdf", encoded)},
		}, nil
	}

	content := string(data)
	lines := strings.Split(content, "\n")
	totalLines := len(lines)

	// Apply offset (1-based; 0 or unset means start from line 1)
	startLine := 1
	if p.Offset > 0 {
		startLine = p.Offset
	}
	if startLine > totalLines {
		return TextResult(fmt.Sprintf("(file has %d lines, offset %d is past end)", totalLines, startLine)), nil
	}

	// Apply limit (0 or unset means default 2000)
	limit := 2000
	if p.Limit > 0 {
		limit = p.Limit
	}

	endLine := startLine - 1 + limit // inclusive index into 0-based slice
	truncated := false
	if endLine > totalLines {
		endLine = totalLines
	} else if endLine < totalLines {
		truncated = true
	}

	selected := lines[startLine-1 : endLine]

	// Determine line-number width for alignment
	maxNum := endLine
	width := len(fmt.Sprintf("%d", maxNum))
	if width < 4 {
		width = 4
	}
	fmtStr := fmt.Sprintf("%%%dd\t%%s\n", width)

	var out strings.Builder
	for i, line := range selected {
		fmt.Fprintf(&out, fmtStr, startLine+i, line)
	}
	if truncated {
		remaining := totalLines - endLine
		fmt.Fprintf(&out, "... (%d lines remaining)\n", remaining)
	}
	return TextResult(out.String()), nil
}

func writeFile(ctx context.Context, params json.RawMessage, store *secrets.Store, baseDir string, blockedPaths ...[]config.BlockedPath) (ToolResult, error) {
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	resolved, err := resolveAndValidatePath(p.Path, baseDir)
	if err != nil {
		return ToolResult{}, err
	}

	if err := checkBlockedPath(store, resolved); err != nil {
		return ToolResult{}, err
	}

	if len(blockedPaths) > 0 {
		if rebuke, blocked := checkConfigBlockedPath(blockedPaths[0], resolved); blocked {
			return TextResult(rebuke), nil
		}
	}

	if err := os.WriteFile(resolved, []byte(p.Content), 0644); err != nil {
		return ToolResult{}, fmt.Errorf("write file: %w", err)
	}

	return TextResult(fmt.Sprintf("Wrote %d bytes to %s", len(p.Content), p.Path)), nil
}

func editFile(ctx context.Context, params json.RawMessage, store *secrets.Store, baseDir string, blockedPaths ...[]config.BlockedPath) (ToolResult, error) {
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	resolved, err := resolveAndValidatePath(p.Path, baseDir)
	if err != nil {
		return ToolResult{}, err
	}

	if err := checkBlockedPath(store, resolved); err != nil {
		return ToolResult{}, err
	}

	if len(blockedPaths) > 0 {
		if rebuke, blocked := checkConfigBlockedPath(blockedPaths[0], resolved); blocked {
			return TextResult(rebuke), nil
		}
	}

	data, err := os.ReadFile(resolved)
	if err != nil {
		return ToolResult{}, fmt.Errorf("read file: %w", err)
	}

	content := string(data)
	count := strings.Count(content, p.OldString)

	if count == 0 {
		return ToolResult{}, fmt.Errorf("old_string not found in %s", p.Path)
	}
	if count > 1 {
		return ToolResult{}, fmt.Errorf("old_string found %d times in %s (must be unique)", count, p.Path)
	}

	preErr := checkSyntax(resolved, data)
	newContent := strings.Replace(content, p.OldString, p.NewString, 1)

	if preErr == nil {
		if postErr := checkSyntax(resolved, []byte(newContent)); postErr != nil {
			return ToolResult{}, fmt.Errorf("edit rejected — would introduce syntax error: %v", postErr)
		}
	}

	if err := os.WriteFile(resolved, []byte(newContent), 0644); err != nil {
		return ToolResult{}, fmt.Errorf("write file: %w", err)
	}

	msg := fmt.Sprintf("Edited %s", p.Path)
	if preErr != nil {
		msg += fmt.Sprintf("\nWarning: file already had syntax errors: %v", preErr)
	}
	return TextResult(msg), nil
}
