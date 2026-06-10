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

// fileScope captures the path-containment context shared by every file-touching
// tool. resolveFileArg is the single choke point: it resolves a caller-supplied
// path (workspace-relative for normal tools), enforces isolated-directory
// containment when baseDir is set, and rejects blocked paths (the secrets file,
// /proc/self/environ, …) via the secrets store. Used by read/write/edit/summary
// and http_request's body_file/files/save_to in every mode so no caller can
// silently skip a check.
type fileScope struct {
	store     *secrets.Store
	baseDir   string // non-empty => isolated: the resolved path must stay within it
	workspace string // base for relative paths in non-isolated tools
}

// resolveFileArg canonicalises path and enforces containment + the blocklist.
// The returned path is safe to hand to os.Open/ReadFile/WriteFile.
func (fs fileScope) resolveFileArg(path string) (string, error) {
	resolved, err := resolveAndValidatePath(resolveWorkspacePath(path, fs.workspace), fs.baseDir)
	if err != nil {
		return "", err
	}
	if err := checkBlockedPath(fs.store, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// rewriteContainedPaths resolves the named path params in raw against fs and
// returns a copy with each rewritten to its safe absolute form. stringFields
// are top-level string params; if arrayField is set, objKey is resolved on each
// object in that array. It lets isolated spawns wrap ExecExport tools (summary,
// http_request) to enforce baseDir containment without re-plumbing their full
// dependency sets — the wrapped tool then operates on already-contained paths.
func rewriteContainedPaths(raw json.RawMessage, fs fileScope, stringFields []string, arrayField, objKey string) (json.RawMessage, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse params: %w", err)
	}
	for _, f := range stringFields {
		if v, ok := m[f].(string); ok && v != "" {
			r, err := fs.resolveFileArg(v)
			if err != nil {
				return nil, err
			}
			m[f] = r
		}
	}
	if arrayField != "" {
		if arr, ok := m[arrayField].([]any); ok {
			for _, it := range arr {
				obj, ok := it.(map[string]any)
				if !ok {
					continue
				}
				if v, ok := obj[objKey].(string); ok && v != "" {
					r, err := fs.resolveFileArg(v)
					if err != nil {
						return nil, err
					}
					obj[objKey] = r
				}
			}
		}
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("re-encode params: %w", err)
	}
	return out, nil
}

func NewReadTool(store *secrets.Store, workspace string) *Tool {
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file (line-numbered) or list a directory. Use offset/limit to read a specific range of lines.",
		Parameters: json.RawMessage(readToolSchema),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return readFile(ctx, params, fileScope{store: store, workspace: workspace})
		},
	}
}

func NewWriteTool(store *secrets.Store, workspace string, blockedPaths []config.BlockedPath, fileMode os.FileMode) *Tool {
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
			return writeFile(ctx, params, fileScope{store: store, workspace: workspace}, fileMode, blockedPaths)
		},
	}
}

func NewEditTool(store *secrets.Store, workspace string, blockedPaths []config.BlockedPath, fileMode os.FileMode) *Tool {
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
			return editFile(ctx, params, fileScope{store: store, workspace: workspace}, fileMode, blockedPaths)
		},
	}
}

func NewIsolatedReadTool(store *secrets.Store, baseDir string) *Tool {
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file (line-numbered) or list a directory. Use offset/limit to read a specific range of lines.",
		Parameters: json.RawMessage(readToolSchema),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return readFile(ctx, params, fileScope{store: store, baseDir: baseDir})
		},
	}
}

func NewIsolatedWriteTool(store *secrets.Store, baseDir string, fileMode os.FileMode) *Tool {
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
			return writeFile(ctx, params, fileScope{store: store, baseDir: baseDir}, fileMode)
		},
	}
}

func NewIsolatedEditTool(store *secrets.Store, baseDir string, fileMode os.FileMode) *Tool {
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
			return editFile(ctx, params, fileScope{store: store, baseDir: baseDir}, fileMode)
		},
	}
}

// expandTilde replaces a leading ~ with the user's home directory.
func expandTilde(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	} else if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

// resolveWorkspacePath resolves a relative path against the agent's workspace directory.
// Absolute paths pass through unchanged. This is NOT a security boundary — it just ensures
// relative paths resolve from the agent's workspace rather than the foci-gw process cwd.
func resolveWorkspacePath(path, workspace string) string {
	path = expandTilde(path)
	if workspace == "" || filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(workspace, path)
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

func readFile(ctx context.Context, params json.RawMessage, fs fileScope) (ToolResult, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	resolved, err := fs.resolveFileArg(p.Path)
	if err != nil {
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

func writeFile(ctx context.Context, params json.RawMessage, fs fileScope, fileMode os.FileMode, blockedPaths ...[]config.BlockedPath) (ToolResult, error) {
	if fileMode == 0 {
		fileMode = 0640
	}
	var p struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	resolved, err := fs.resolveFileArg(p.Path)
	if err != nil {
		return ToolResult{}, err
	}

	if len(blockedPaths) > 0 {
		if rebuke, blocked := checkConfigBlockedPath(blockedPaths[0], resolved); blocked {
			return TextResult(rebuke), nil
		}
	}

	if err := os.WriteFile(resolved, []byte(p.Content), fileMode); err != nil {
		return ToolResult{}, fmt.Errorf("write file: %w", err)
	}

	return TextResult(fmt.Sprintf("Wrote %d bytes to %s", len(p.Content), p.Path)), nil
}

func editFile(ctx context.Context, params json.RawMessage, fs fileScope, fileMode os.FileMode, blockedPaths ...[]config.BlockedPath) (ToolResult, error) {
	if fileMode == 0 {
		fileMode = 0640
	}
	var p struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return ToolResult{}, fmt.Errorf("parse params: %w", err)
	}

	resolved, err := fs.resolveFileArg(p.Path)
	if err != nil {
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

	if err := os.WriteFile(resolved, []byte(newContent), fileMode); err != nil {
		return ToolResult{}, fmt.Errorf("write file: %w", err)
	}

	msg := fmt.Sprintf("Edited %s", p.Path)
	if preErr != nil {
		msg += fmt.Sprintf("\nWarning: file already had syntax errors: %v", preErr)
	}
	return TextResult(msg), nil
}
