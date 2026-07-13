package tools

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"foci/internal/log"
	"foci/internal/procx"
)

// CLISummariser implements Summariser by shelling out to `claude --print`.
// Used by delegated (CC-mode) agents whose parent process is already a
// claude invocation: routing summarisation through the same auth means the
// call charges subscription mana, not separate API spend.
//
// Invocation:
//
//	claude --print --no-session-persistence --model <model>
//	       --system-prompt "<summarisation instructions>"
//
// stdin receives the formatted user message (content + prompt). stdout is
// the model's text response. --no-session-persistence avoids leaving a JSONL
// file behind. --system-prompt replaces the default system prompt, so
// CLAUDE.md auto-discovery and dynamic system-prompt sections (cwd, env, git
// status) are skipped automatically.
//
// Note: --bare would skip more (hooks, LSP, plugins, keychain), but it also
// disables OAuth auth — making the CLI fall back to ANTHROPIC_API_KEY only,
// which defeats the purpose of routing through the subscription. Accepting
// hooks/LSP/plugin overhead is the price of subscription-mana auth.
type CLISummariser struct {
	binary        string     // path to claude binary; "claude" by default
	model         string     // model alias (e.g. "haiku")
	maxInputChars func() int // 0 disables cap; called fresh per Summarise
}

// NewCLISummariser builds the CLI-path summariser.
//
// binary is the path to the claude executable; pass "" to use $PATH lookup.
// model is the Claude model alias (e.g. "haiku"); pass "" for "haiku".
func NewCLISummariser(binary, model string, maxInputChars func() int) *CLISummariser {
	if binary == "" {
		binary = "claude"
	}
	if model == "" {
		model = "haiku"
	}
	return &CLISummariser{
		binary:        binary,
		model:         model,
		maxInputChars: maxInputChars,
	}
}

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

// Summarise spawns `claude --print ...` with stdin = content+prompt envelope
// and returns the captured stdout.
func (s *CLISummariser) Summarise(ctx context.Context, content []byte, prompt, filePath string) (string, error) {
	content = CapInputChars(content, s.maxInputChars())

	text, err := CLIOneShot(ctx, s.binary, s.model, summarySystemPrompt, []byte(summaryUserMessage(content, prompt, filePath)))
	if err != nil {
		return "", err
	}

	sessionKey := SessionKeyFromContext(ctx)
	log.Infof("summary", "session=%s transport=cli model=%s input_bytes=%d output_bytes=%d",
		sessionKey, s.model, len(content), len(text))

	if text == "" {
		return "(empty response)", nil
	}
	return text, nil
}
