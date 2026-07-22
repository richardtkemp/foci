package tools

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// BatchRunner is the minimal one-shot capability BatchSummariser needs from
// the agent's DelegatedManager (foci/internal/agent). It is declared here
// narrowly — matching delegator.BatchRunner's own RunBatch shape rather than
// depending on *agent.DelegatedManager directly — because internal/agent
// already imports internal/tools (for the exec bridge); importing agent back
// from tools would cycle. DelegatedManager satisfies this interface
// structurally via its own RunBatch method (a thin wrapper around dispatching
// to the agent's configured backend's delegator.BatchRunner).
type BatchRunner interface {
	RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error)
}

// BatchSummariser implements Summariser by dispatching through the agent's
// own DelegatedManager.RunBatch — i.e. through whichever backend
// (claude-code, codex, opencode) the agent is actually configured to use.
//
// This replaces CLISummariser as the delegated-agent summariser: CLISummariser
// always shelled `claude --print` directly, so a delegated agent configured
// for codex or opencode still ran its foci_summary calls on the claude CLI
// (foci_todo #1317) — silently mismatched auth/billing/model family from the
// agent's actual backend, exactly the bug #1312 fixed for RunOnce's other
// three consumers (nudge extraction, memory consolidation, onboarding).
type BatchSummariser struct {
	// runner is a lazy accessor rather than a captured value: the tool
	// registry (and this summariser) is built by buildExecRegistry BEFORE
	// ag.DelegatedManager is assigned (cmd/foci-gw/agents_delegated.go,
	// configureDelegated) — capturing the manager at construction time would
	// capture nil. By the time Summarise() actually executes at runtime,
	// agent setup has long finished and the manager is live.
	runner func() BatchRunner

	model         string // batch model preference, e.g. "haiku"
	workDir       string
	agentID       string
	maxInputChars func() int // 0 disables cap; called fresh per Summarise
}

// NewBatchSummariser builds the batch-dispatch summariser. runner is called
// fresh on each Summarise to resolve the current BatchRunner (nil if the
// agent's delegation isn't wired up yet — reported as an error, not a panic).
// model is the cheap-batch model preference (e.g. "haiku"); workDir/agentID
// populate the delegator.BatchRequest the same way DelegatedManager.RunOnce
// does for its own callers.
func NewBatchSummariser(runner func() BatchRunner, model, workDir, agentID string, maxInputChars func() int) *BatchSummariser {
	return &BatchSummariser{
		runner:        runner,
		model:         model,
		workDir:       workDir,
		agentID:       agentID,
		maxInputChars: maxInputChars,
	}
}

// Summarise dispatches the content+prompt envelope through the resolved
// BatchRunner and returns its trimmed text response.
func (s *BatchSummariser) Summarise(ctx context.Context, content []byte, prompt, filePath string) (string, error) {
	content = CapInputChars(content, s.maxInputChars())

	if s.runner == nil {
		return "", fmt.Errorf("batch summariser: no runner configured")
	}
	br := s.runner()
	if br == nil {
		return "", fmt.Errorf("batch summariser: no BatchRunner available (delegated manager not yet configured)")
	}

	result, err := br.RunBatch(ctx, delegator.BatchRequest{
		Prompt:       summaryUserMessage(content, prompt, filePath),
		SystemPrompt: summarySystemPrompt,
		Model:        s.model,
		WorkDir:      s.workDir,
		AgentID:      s.agentID,
	})
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(result)

	sessionKey := SessionKeyFromContext(ctx)
	summaryLog.Infof("session=%s transport=batch model=%s input_bytes=%d output_bytes=%d",
		sessionKey, s.model, len(content), len(text))

	if text == "" {
		return "(empty response)", nil
	}
	return text, nil
}
