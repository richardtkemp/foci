package ccstream

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"foci/internal/delegator"
	"foci/internal/procx"
	"foci/internal/tempdir"
)

// RunBatch implements delegator.BatchRunner: a one-shot `claude --print`
// invocation reusing the parent process's subscription auth (OAuth), so the
// call charges mana rather than separate API spend. Runs on an unstarted
// Backend instance — only cfg (binary override) is consulted.
//
// --no-session-persistence avoids leaving a JSONL file behind; --system-prompt-file
// (when set) replaces the default system prompt, which also skips CLAUDE.md
// auto-discovery and the dynamic cwd/env/git sections. Do NOT add --bare: it
// disables OAuth and forces ANTHROPIC_API_KEY, defeating subscription auth.
//
// The system prompt is ALWAYS written to a file and passed via
// --system-prompt-file, never as a literal --system-prompt argv element
// (#1963). A composed character system prompt is caller-controlled and
// unbounded — coach's alone is 131,575 chars and growing — while Linux caps
// a single execve() argv element at MAX_ARG_STRLEN (32 pages = 131072
// bytes), independent of the total-argv limit and NOT raised by ulimit.
// Crossing it fails fork/exec with E2BIG ("argument list too long") before
// claude ever starts, which retries cannot fix since every attempt hits the
// same cliff. cctmux's Start already uses the file form for the same reason
// (see cctmux/lifecycle.go); this brings RunBatch in line so nudge
// extraction and memory consolidation (both routed through here) can't hit
// it either, for any agent, at any size.
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
		promptFile, cleanup, err := writeSystemPromptFile(req.SystemPrompt)
		if err != nil {
			return "", fmt.Errorf("write system prompt file: %w", err)
		}
		defer cleanup()
		args = append(args, "--system-prompt-file", promptFile)
	}

	// RunWithETXTBSYRetry rebuilds the Cmd (fresh stdin reader, reset buffers)
	// each attempt to survive a transient "text file busy" from concurrent
	// fork/exec (golang/go#22315). Safe to retry: `claude --print` has no
	// observable side effects until the child actually execs.
	var stdout, stderr bytes.Buffer
	err := procx.RunWithETXTBSYRetry(ctx, func() *exec.Cmd {
		stdout.Reset()
		stderr.Reset()
		// Operator: a Claude Code process — the canonical agent population.
		cmd := procx.Spawn(ctx, procx.Operator, b.resolveBinary(), args...)
		cmd.Dir = req.WorkDir
		cmd.Stdin = strings.NewReader(req.Prompt)
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		return cmd
	})
	if err != nil {
		if d := procx.FailureDetail(stderr.String(), stdout.String()); d != "" {
			return "", fmt.Errorf("claude --print failed: %w (%s)", err, d)
		}
		return "", fmt.Errorf("claude --print failed: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// writeSystemPromptFile writes prompt to a private (0600) temp file under
// foci's temp root and returns its path plus a cleanup func the caller
// should defer. Keeping this out of the caller's WorkDir (which for RunBatch
// is often a persistent agent workspace) means concurrent/overlapping batch
// runs never collide on the same filename.
func writeSystemPromptFile(prompt string) (path string, cleanup func(), err error) {
	f, err := tempdir.Create("system-prompt-*")
	if err != nil {
		return "", nil, err
	}
	path = f.Name()
	cleanup = func() { _ = os.Remove(path) }
	if _, err := f.WriteString(prompt); err != nil {
		_ = f.Close()
		cleanup()
		return "", nil, err
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, err
	}
	return path, cleanup, nil
}
