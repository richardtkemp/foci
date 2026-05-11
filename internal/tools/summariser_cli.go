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
	binary        string // path to claude binary; "claude" by default
	model         string // model alias (e.g. "haiku")
	maxInputChars int    // 0 disables cap
}

// NewCLISummariser builds the CLI-path summariser.
//
// binary is the path to the claude executable; pass "" to use $PATH lookup.
// model is the Claude model alias (e.g. "haiku"); pass "" for "haiku".
// maxInputChars caps content size; pass 0 to disable.
func NewCLISummariser(binary, model string, maxInputChars int) *CLISummariser {
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

// Summarise spawns `claude --print ...` with stdin = content+prompt envelope
// and returns the captured stdout. Errors include the subprocess's stderr to
// aid debugging when the CLI fails (auth, model unavailable, etc.).
func (s *CLISummariser) Summarise(ctx context.Context, content []byte, prompt, filePath string) (string, error) {
	content = CapInputChars(content, s.maxInputChars)

	cmd := procx.Spawn(ctx, s.binary,
		"--print",
		"--no-session-persistence",
		"--model", s.model,
		"--system-prompt", summarySystemPrompt,
	)
	cmd.Stdin = bytes.NewReader([]byte(summaryUserMessage(content, prompt, filePath)))

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		// Surface stderr in the error so call sites can debug auth/model issues.
		stderrStr := strings.TrimSpace(stderr.String())
		if stderrStr != "" {
			return "", fmt.Errorf("claude --print failed: %w (stderr: %s)", err, stderrStr)
		}
		return "", fmt.Errorf("claude --print failed: %w", err)
	}

	sessionKey := SessionKeyFromContext(ctx)
	log.Infof("summary", "session=%s transport=cli model=%s input_bytes=%d output_bytes=%d",
		sessionKey, s.model, len(content), stdout.Len())

	text := strings.TrimSpace(stdout.String())
	if text == "" {
		return "(empty response)", nil
	}
	return text, nil
}
