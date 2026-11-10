package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CompactionRequester records that the session wants its context compacted
// once its current turn ends. Satisfied by agent.Agent.RequestCompaction;
// injected so the tool is testable without an agent.
type CompactionRequester func(sessionKey string) error

// NewCompactSessionTool creates the compact_session tool (#3040): the agent
// asks foci to compact its own conversation. The compaction runs after the
// current turn, once the turn's final reply is out (Dick 2026-11-10: "After
// the turn."), on every backend, and also while subagents or background
// commands run; a Claude Code restart that would kill them waits until they
// finish.
func NewCompactSessionTool(request CompactionRequester) *Tool {
	return &Tool{
		Name: "compact_session",
		Description: "Compact this conversation's context: the history so far is replaced by a summary, the same as the user's /compact. " +
			"It runs after your current turn ends, once your final reply is delivered, so finish what you are saying first. " +
			"It also runs while subagents or background commands run, and does not kill them. " +
			"Use it when the context is large and the earlier detail is no longer needed (a task is finished, a new one starts). " +
			"Write anything you must keep to a file first: the summary does not keep every detail.",
		ExecExport: true,
		Parameters: json.RawMessage(`{"type":"object","properties":{}}`),
		Execute: func(ctx context.Context, _ json.RawMessage) (ToolResult, error) {
			if IsSpawnCharacter(ctx) {
				// A one-shot character spawn's tool calls run on its parent's
				// context, so this would compact the PARENT's conversation.
				return ToolResult{}, errors.New("compact_session is not available in a character spawn: it would compact your spawner's conversation")
			}
			sk := SessionKeyFromContext(ctx)
			if sk == "" {
				return ToolResult{}, errors.New("no session key in context")
			}
			if err := request(sk); err != nil {
				return ToolResult{}, fmt.Errorf("cannot request compaction: %w", err)
			}
			return TextResult("Compaction requested. It runs when this turn ends, after your final reply is delivered; " +
				"the chat shows its progress. Running subagents and background commands are not killed."), nil
		},
	}
}
