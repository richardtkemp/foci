package tools

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"foci/internal/procx"
)

// CLIOneShot runs `claude --print` as a one-shot and returns its trimmed
// stdout. It reuses the parent claude process's subscription auth (OAuth), so
// the call charges mana rather than separate API spend — this is how delegated
// (claude-code) agents make cheap auxiliary LLM calls (summaries, prompt diffs)
// with no API client, model resolver, or anthropic credentials.
//
// binary "" → "claude" (PATH lookup); model "" → "haiku". systemPrompt replaces
// the default system prompt (skipping CLAUDE.md auto-discovery and the dynamic
// cwd/env/git sections); userMessage is piped to stdin. Errors include the
// subprocess's stderr to aid debugging (auth, model unavailable, etc.).
//
// Do NOT add --bare: it disables OAuth and forces ANTHROPIC_API_KEY, defeating
// the subscription-auth purpose.
func CLIOneShot(ctx context.Context, binary, model, systemPrompt string, userMessage []byte) (string, error) {
	if binary == "" {
		binary = "claude"
	}
	if model == "" {
		model = "haiku"
	}
	cmd := procx.Spawn(ctx, binary,
		"--print",
		"--no-session-persistence",
		"--model", model,
		"--system-prompt", systemPrompt,
	)
	cmd.Stdin = bytes.NewReader(userMessage)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if stderrStr := strings.TrimSpace(stderr.String()); stderrStr != "" {
			return "", fmt.Errorf("claude --print failed: %w (stderr: %s)", err, stderrStr)
		}
		return "", fmt.Errorf("claude --print failed: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}
