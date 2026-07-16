package ccstream

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"foci/internal/delegator"
	"foci/internal/procx"
)

// RunBatch implements delegator.BatchRunner: a one-shot `claude --print`
// invocation reusing the parent process's subscription auth (OAuth), so the
// call charges mana rather than separate API spend. Runs on an unstarted
// Backend instance — only cfg (binary override) is consulted.
//
// --no-session-persistence avoids leaving a JSONL file behind; --system-prompt
// (when set) replaces the default system prompt, which also skips CLAUDE.md
// auto-discovery and the dynamic cwd/env/git sections. Do NOT add --bare: it
// disables OAuth and forces ANTHROPIC_API_KEY, defeating subscription auth.
func (b *Backend) RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error) {
	model := req.Model
	if model == "" {
		model = "sonnet"
	}
	args := []string{
		"--print",
		"--dangerously-skip-permissions",
		"--no-session-persistence",
		"--model", model,
	}
	if req.SystemPrompt != "" {
		args = append(args, "--system-prompt", req.SystemPrompt)
	}

	cmd := procx.Spawn(ctx, b.resolveBinary(), args...)
	cmd.Dir = req.WorkDir
	cmd.Stdin = strings.NewReader(req.Prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if s := strings.TrimSpace(stderr.String()); s != "" {
			return "", fmt.Errorf("claude --print failed: %w (stderr: %s)", err, s)
		}
		return "", fmt.Errorf("claude --print failed: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
