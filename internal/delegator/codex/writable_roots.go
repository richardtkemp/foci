package codex

import (
	"path/filepath"
	"slices"
	"strings"

	"foci/internal/delegator"
)

// writableRoots returns a non-nil list so start/fork can clear inherited grants.
// Read-only exploration and other sandbox modes receive no grants.
func (b *Backend) writableRoots() []string {
	roots := []string{}
	if b.sandboxMode() == "workspace-write" {
		if configured, ok := b.cfg[delegator.WritableRootsConfigKey].([]string); ok {
			roots = append(roots, configured...)
		}
	}
	return roots
}

// writableRootsConfig applies roots on start/fork. Resume receives the override
// too, but Codex 0.166.0 retains saved roots (noteResumedWritableRoots diagnoses it).
// Using config overrides preserves Codex's own network and temporary-dir defaults
// instead of reconstructing a sandboxPolicy with different implicit values.
func (b *Backend) writableRootsConfig() map[string]any {
	return map[string]any{"sandbox_workspace_write.writable_roots": b.writableRoots()}
}

// appServerArgs applies the same root list to command/exec and thread defaults.
// Quote each path as TOML (not shell syntax), including spaces and backslashes.
func (b *Backend) appServerArgs(compactPrompt string) []string {
	args := appServerArgs(compactPrompt, b.sandboxMode(), b.approvalsReviewer(), b.approvalPolicy())
	var quoted []string
	for _, root := range b.writableRoots() {
		quoted = append(quoted, tomlBasicString(root))
	}
	return append(args, "-c", "sandbox_workspace_write.writable_roots=["+strings.Join(quoted, ",")+"]")
}

// noteResumedWritableRoots diagnoses persisted grants without discarding history.
// Compare sets of clean paths: ordering, duplicates and trailing slashes do not
// change access. This is informational, not an approval or sandbox-mode change.
func (b *Backend) noteResumedWritableRoots(threadID string, policy *sandboxPolicy) {
	if policy == nil || policy.Type != "workspaceWrite" || b.sandboxMode() != "workspace-write" {
		return
	}
	canonical := func(roots []string) []string {
		result := make([]string, len(roots))
		for i, root := range roots {
			result[i] = filepath.Clean(root)
		}
		slices.Sort(result)
		return slices.Compact(result)
	}
	configured, effective := b.writableRoots(), policy.WritableRoots
	if slices.Equal(canonical(configured), canonical(effective)) {
		return
	}
	b.mu.Lock()
	if b.staleRootsLogged[threadID] {
		b.mu.Unlock()
		return
	}
	if b.staleRootsLogged == nil {
		b.staleRootsLogged = make(map[string]bool)
	}
	b.staleRootsLogged[threadID] = true
	b.mu.Unlock()
	b.logInfof("resumed thread %s: writable roots differ: effective=%q configured=%q; Codex retains saved grants on resume; start a new thread or fork to apply the current setting", threadID, effective, configured)
}
