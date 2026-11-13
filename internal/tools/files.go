package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"foci/internal/config"
	"foci/internal/provider"
	"foci/internal/secrets"
	"foci/internal/tempdir"
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
// path (workspace-relative for normal tools), canonicalises it (symlinks
// resolved), confines it, and rejects blocked paths (the secrets file,
// /proc/self/environ, …) via the secrets store. Used by read/write/edit/summary,
// send_to_chat's file and http_request's body_file/files/save_to in every mode,
// so no caller can silently skip a check.
//
// Confinement (#3076): an isolated scope (baseDir set) keeps the path inside
// baseDir. Every other scope keeps it inside the allowed areas: the agent's
// workspace, /tmp (and the process temp dir and foci's temp root), and the
// operator's tools.file_allowed_paths. These tools run inside foci-gw and are
// auto-approved, so without this an agent could upload or overwrite any file
// the foci user can reach without the approval its shell would need.
type fileScope struct {
	store     *secrets.Store
	baseDir   string // non-empty => isolated: the resolved path must stay within it
	workspace string // base for relative paths, and an allowed area, in non-isolated tools
	// extraRoots returns the live tools.file_allowed_paths for the agent. A
	// func so a config edit applies on the next call. Nil means none.
	extraRoots func() []string
	// tempRoots overrides the built-in temp areas when non-nil. Tests only:
	// every writable test dir lies under a temp area, so a test that needs an
	// "outside" path removes them.
	tempRoots *[]string
	// confineAreas, when set and returning false, skips the allowed-area
	// check: the path is still canonicalised and the blocklist still applies.
	// Read on each call, so a live config edit applies (#3100: an API explore
	// spawn's read, confined only when tools.explore_confine_reads is on).
	// Nil means always confine. Isolated scopes ignore it.
	confineAreas func() bool
	// maxReadBytes returns the current cap on a file's size before read/edit
	// loads it; <=0 uses the default. A func (not a captured scalar) so the
	// read/edit tools observe a live config edit on their next call instead of
	// freezing the value from tool construction. Nil (e.g. isolated-spawn
	// scopes and tests that construct a fileScope directly) uses the default.
	maxReadBytes func() int64
}

// FileOpt configures the file scope of a non-isolated file-touching tool
// (#3076). The constructors take them variadically so a caller that passes
// none still gets confinement (to the temp areas only).
type FileOpt func(*fileScope)

// WithWorkspace sets the agent workspace: the base for relative paths and an
// allowed area.
func WithWorkspace(dir string) FileOpt { return func(fs *fileScope) { fs.workspace = dir } }

// WithExtraRoots sets the live source of tools.file_allowed_paths.
func WithExtraRoots(fn func() []string) FileOpt {
	return func(fs *fileScope) { fs.extraRoots = fn }
}

// WithAreaConfinement makes the allowed-area check conditional (#3100): fn is
// read on each call, and false lets the path lie anywhere the foci user can
// reach (canonicalised, blocklist still applied).
func WithAreaConfinement(fn func() bool) FileOpt {
	return func(fs *fileScope) { fs.confineAreas = fn }
}

// WithBlocklist sets the secrets store whose blocklist the tool enforces.
func WithBlocklist(store *secrets.Store) FileOpt { return func(fs *fileScope) { fs.store = store } }

func newFileScope(base fileScope, opts []FileOpt) fileScope {
	for _, o := range opts {
		if o != nil {
			o(&base)
		}
	}
	return base
}

// FileAllowedPathsKey is the config key named in a refusal.
const FileAllowedPathsKey = "tools.file_allowed_paths"

// readLimit returns the effective max file size the read/edit tools will load,
// falling back to the package default when unset.
func (fs fileScope) readLimit() int64 {
	if fs.maxReadBytes != nil {
		if v := fs.maxReadBytes(); v > 0 {
			return v
		}
	}
	return config.DefaultMaxFileReadBytes
}

// resolveFileArg canonicalises path and enforces containment + the blocklist.
// The returned path is the canonical (symlink-resolved) one, safe to hand to
// os.Open/ReadFile/WriteFile: the caller opens exactly the path that was checked.
func (fs fileScope) resolveFileArg(path string) (string, error) {
	var resolved string
	var err error
	if fs.baseDir != "" {
		resolved, err = resolveAndValidatePath(resolveWorkspacePath(path, fs.workspace), fs.baseDir)
	} else if fs.confineAreas != nil && !fs.confineAreas() {
		resolved, err = canonicalFilePath(resolveWorkspacePath(path, fs.workspace))
	} else {
		resolved, err = fs.confine(resolveWorkspacePath(path, fs.workspace))
	}
	if err != nil {
		return "", err
	}
	if err := checkBlockedPath(fs.store, resolved); err != nil {
		return "", err
	}
	return resolved, nil
}

// allowedRoots returns the canonical allowed areas, in display order, without
// duplicates: the workspace, /tmp, the process temp dir, foci's temp root, then
// the configured extras.
func (fs fileScope) allowedRoots() []string {
	temps := []string{"/tmp", os.TempDir(), tempdir.Dir()}
	if fs.tempRoots != nil {
		temps = *fs.tempRoots
	}
	cands := append([]string{fs.workspace}, temps...)
	if fs.extraRoots != nil {
		cands = append(cands, fs.extraRoots()...)
	}
	var roots []string
	for _, c := range cands {
		if c == "" || !filepath.IsAbs(c) {
			continue // a relative root has no fixed meaning here: never matches
		}
		r, err := canonicalFilePath(c)
		if err != nil {
			r = filepath.Clean(c)
		}
		if !slices.Contains(roots, r) {
			roots = append(roots, r)
		}
	}
	return roots
}

// confine canonicalises path and requires it to lie in an allowed area.
func (fs fileScope) confine(path string) (string, error) {
	canon, roots, inside, err := fs.locate(path)
	if err != nil {
		return "", err
	}
	if inside {
		return canon, nil
	}
	return "", fmt.Errorf("access denied: %s is outside the areas foci file tools may use (%s); "+
		"the agent's workspace and /tmp are always allowed, and an operator can add a directory with %s in foci.toml",
		shownPath(path, canon), strings.Join(roots, ", "), FileAllowedPathsKey)
}

// locate canonicalises path and reports whether it lies in an allowed area.
// It also returns the allowed areas, so each caller words its own refusal
// (the file tools in confine, the ask grader in graderScope.resolve).
func (fs fileScope) locate(path string) (canon string, roots []string, inside bool, err error) {
	canon, err = canonicalFilePath(path)
	if err != nil {
		return "", nil, false, err
	}
	roots = fs.allowedRoots()
	for _, r := range roots {
		if withinDir(canon, r) {
			return canon, roots, true, nil
		}
	}
	return canon, roots, false, nil
}

// shownPath renders path for a refusal, with its canonical form when the two
// differ (a symlink was resolved).
func shownPath(path, canon string) string {
	if canon != path {
		return fmt.Sprintf("%s (resolves to %s)", path, canon)
	}
	return path
}

// withinDir reports whether p is root or lies under it. Both must be canonical.
func withinDir(p, root string) bool {
	if root == string(filepath.Separator) {
		return true
	}
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// canonicalFilePath returns the absolute, cleaned, symlink-resolved form of p.
// For a path that does not exist yet it resolves the deepest existing ancestor
// and re-appends the rest. Unlike secrets.CanonicalPath it fails closed: an
// error other than "does not exist" is returned, and a dangling symlink
// anywhere in the unresolved tail is refused — os.WriteFile would follow it
// to a target the containment check never saw.
func canonicalFilePath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	r, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return r, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	dir := abs
	var tail []string
	for {
		// dir does not resolve. If it exists at all, it is a dangling link.
		if _, lerr := os.Lstat(dir); lerr == nil {
			return "", fmt.Errorf("access denied: %s is a symlink to a missing target", dir)
		} else if !os.IsNotExist(lerr) {
			return "", fmt.Errorf("resolve path: %w", lerr)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs, nil // unreachable in practice: the root always exists
		}
		tail = append(tail, filepath.Base(dir))
		dir = parent
		r, err := filepath.EvalSymlinks(dir)
		if err == nil {
			for i := len(tail) - 1; i >= 0; i-- {
				r = filepath.Join(r, tail[i])
			}
			return r, nil
		}
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve ancestor: %w", err)
		}
	}
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

func NewReadTool(store *secrets.Store, workspace string, maxReadBytes func() int64, opts ...FileOpt) *Tool {
	scope := newFileScope(fileScope{store: store, workspace: workspace, maxReadBytes: maxReadBytes}, opts)
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file (line-numbered) or list a directory. Use offset/limit to read a specific range of lines.",
		Parameters:  json.RawMessage(readToolSchema),
		Execute: func(ctx context.Context, params json.RawMessage) (ToolResult, error) {
			return readFile(ctx, params, scope)
		},
	}
}

func NewWriteTool(store *secrets.Store, workspace string, blockedPaths []config.BlockedPath, fileMode os.FileMode, opts ...FileOpt) *Tool {
	scope := newFileScope(fileScope{store: store, workspace: workspace}, opts)
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
			return writeFile(ctx, params, scope, fileMode, blockedPaths)
		},
	}
}

func NewEditTool(store *secrets.Store, workspace string, blockedPaths []config.BlockedPath, fileMode os.FileMode, maxReadBytes func() int64, opts ...FileOpt) *Tool {
	scope := newFileScope(fileScope{store: store, workspace: workspace, maxReadBytes: maxReadBytes}, opts)
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
			return editFile(ctx, params, scope, fileMode, blockedPaths)
		},
	}
}

func NewIsolatedReadTool(store *secrets.Store, baseDir string) *Tool {
	return &Tool{
		Name:        "read",
		Description: "Read the contents of a file (line-numbered) or list a directory. Use offset/limit to read a specific range of lines.",
		Parameters:  json.RawMessage(readToolSchema),
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

// expandTilde replaces a leading ~ with the user's home directory ($HOME, as
// the agent's shell expands it; not foci's install root, #2580).
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

	resolved, err := canonicalFilePath(filepath.Join(baseDir, path))
	if err != nil {
		return "", err
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

// imageMimeForExt maps a file extension to an image MIME type, or "" if the
// extension isn't a supported image (#630). Kept in sync with the model
// providers' accepted image formats.
func imageMimeForExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	default:
		return ""
	}
}

func readFile(ctx context.Context, params json.RawMessage, fs fileScope) (ToolResult, error) {
	p, err := UnmarshalParams[struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}](params)
	if err != nil {
		return ToolResult{}, err
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

	// Reject oversized files by their stat size before loading them whole into
	// memory (large-file OOM guard).
	if limit := fs.readLimit(); info.Size() > limit {
		return ToolResult{}, fmt.Errorf("file too large: %d bytes (max %d) — use offset/limit or a streaming tool", info.Size(), limit)
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

	// Image files: base64-encode and return as an image content block so the
	// model can actually see them (#630) — the same way platform-received images
	// are delivered. Non-image files fall through to text handling below.
	if mime := imageMimeForExt(filepath.Ext(resolved)); mime != "" {
		const maxImageSize = 8 * 1024 * 1024 // provider image-payload ceiling
		if len(data) > maxImageSize {
			return ToolResult{}, fmt.Errorf("image too large: %d bytes (max %d)", len(data), maxImageSize)
		}
		encoded := base64.StdEncoding.EncodeToString(data)
		return ToolResult{
			Text:        fmt.Sprintf("[Image: %s, %d bytes]", filepath.Base(resolved), len(data)),
			ExtraBlocks: []provider.ContentBlock{provider.ImageBlock(mime, encoded)},
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
	p, err := UnmarshalParams[struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}](params)
	if err != nil {
		return ToolResult{}, err
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
	p, err := UnmarshalParams[struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}](params)
	if err != nil {
		return ToolResult{}, err
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

	// Reject oversized files by their stat size before loading them whole into
	// memory (large-file OOM guard).
	if info, err := os.Stat(resolved); err != nil {
		return ToolResult{}, fmt.Errorf("edit file: %w", err)
	} else if limit := fs.readLimit(); info.Size() > limit {
		return ToolResult{}, fmt.Errorf("file too large to edit: %d bytes (max %d)", info.Size(), limit)
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
