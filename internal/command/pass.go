package command

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/tools"
)

// PassCommand creates a /pass command that forwards raw text directly to the
// configured delegated backend. This bypasses foci's command dispatch,
// allowing users to run backend-native slash commands that would otherwise
// be intercepted by foci (e.g., /context, /model, /compact).
//
// For tmux-based backends, /pass captures the pane output after the command
// stabilises and returns it. For stream backends, local command output arrives
// via the stdout reader and is delivered normally.
//
// Only available for agents with a delegated backend — returns an error for
// API-mode agents where there's no backend to forward to.
//
// Usage: /pass /context
//
//	/pass /model opus
//	/pass /help
//
// backendType is the configured [agents].backend value (e.g. "claude-code",
// "codex"), used only to name the backend in user-facing text — it does not
// change dispatch behaviour, which still goes through DelegatedManager.
func PassCommand(backendType string) *Command {
	backendName := delegator.HumanReadableBackendName(backendType)
	return &Command{
		Name:        "pass",
		Description: fmt.Sprintf("Forward a command directly to %s", backendName),
		Category:    "operations",
		Execute: func(ctx context.Context, req Request, cc CommandContext) (Response, error) {
			if cc.Agent.DelegatedManager == nil {
				return Response{}, fmt.Errorf("/pass is only available for agents with a delegated backend (%s)", backendName)
			}

			if req.Args == "" {
				return Response{}, fmt.Errorf("usage: /pass <command>\nexample: /pass /context")
			}

			sk := tools.SessionKeyFromContext(ctx)
			if sk == "" {
				sk = req.SessionKey
			}
			if sk == "" {
				return Response{}, fmt.Errorf("no active session")
			}

			be, err := cc.Agent.DelegatedManager.Get(ctx, sk)
			if err != nil {
				return Response{}, fmt.Errorf("get backend: %w", err)
			}

			if err := be.ImmediateInject(ctx, delegator.Inject{
				Source: delegator.SourcePass,
				Text:   req.Args,
			}); err != nil {
				return Response{}, fmt.Errorf("send command: %w", err)
			}

			// For backends that support pane capture (tmux), wait for the
			// output to stabilise and return it. Local slash commands don't
			// write to the JSONL so the watcher won't deliver them.
			if capturer, ok := be.(delegator.CommandOutputCapturer); ok {
				raw, err := capturer.CaptureCommandOutput(ctx, 1*time.Second, 200*time.Millisecond)
				if err == nil && raw != "" {
					output := extractCommandOutput(raw, req.Args)
					if output != "" {
						return Response{Text: output}, nil
					}
				}
			}

			return Response{Text: fmt.Sprintf("↗ Sent to %s: `%s`", backendName, req.Args)}, nil
		},
	}
}

// extractCommandOutput parses tmux pane content to extract just the output
// from a slash command. The pane shows:
//
//	❯ /command
//	  ⎿  output lines...
//	─────────────────
//	❯
//
// We extract everything between the command line and the final prompt.
func extractCommandOutput(paneContent, command string) string {
	lines := strings.Split(paneContent, "\n")

	// Find the command line (last occurrence of "❯ <command>").
	cmdStart := -1
	for i := len(lines) - 1; i >= 0; i-- {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "❯") && strings.Contains(trimmed, strings.TrimPrefix(command, "/")) {
			cmdStart = i
			break
		}
	}
	if cmdStart < 0 {
		return ""
	}

	// Find the next prompt line after the command (bare "❯" or "❯ " with no command).
	cmdEnd := -1
	for i := cmdStart + 1; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed == "❯" || trimmed == "❯ " {
			cmdEnd = i
			break
		}
	}
	if cmdEnd < 0 {
		cmdEnd = len(lines)
	}

	// Extract lines between command and next prompt, stripping separator lines.
	var output []string
	for i := cmdStart + 1; i < cmdEnd; i++ {
		line := lines[i]
		// Skip separator lines (all ─ characters).
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if isSeparatorLine(trimmed) {
			continue
		}
		output = append(output, line)
	}

	if len(output) == 0 {
		return ""
	}

	result := strings.Join(output, "\n")
	return strings.TrimSpace(result)
}

// isSeparatorLine checks if a line is all box-drawing characters (─).
func isSeparatorLine(s string) bool {
	for _, r := range s {
		if r != '─' && r != '━' && r != '-' {
			return false
		}
	}
	return len(s) > 10 // must be long enough to be a separator, not just a dash
}
