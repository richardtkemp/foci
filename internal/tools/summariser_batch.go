package tools

import (
	"context"
	"fmt"
	"strings"

	"foci/internal/delegator"
)

// BatchRunner is the minimal one-shot capability BatchSummariser needs from
// the agent's DelegatedManager (foci/internal/agent). It is declared here
// narrowly rather than depending on *agent.DelegatedManager directly because
// internal/agent already imports internal/tools (for the exec bridge);
// importing agent back from tools would cycle. DelegatedManager satisfies it
// structurally via RunBatch, which runs the request as an ordinary turn on an
// ephemeral child of the calling session (#1962).
type BatchRunner interface {
	RunBatch(ctx context.Context, req delegator.BatchRequest) (string, error)
}

// BatchSummariser implements Summariser by dispatching through the agent's
// own DelegatedManager.RunBatch — i.e. through whichever backend
// (claude-code, codex, opencode) the agent is actually configured to use.
//
// It never names a model on its own: a model name is backend-specific, so it
// asks RunBatch for the backend's cheap tier (delegator.BatchRequest.Cheap)
// unless the operator configured one ([tools] summary_model, #2032).
type BatchSummariser struct {
	// runner is a lazy accessor rather than a captured value: the tool
	// registry (and this summariser) is built by buildExecRegistry BEFORE
	// ag.DelegatedManager is assigned (cmd/foci-gw/agents_delegated.go,
	// configureDelegated) — capturing the manager at construction time would
	// capture nil. By the time Summarise() actually executes at runtime,
	// agent setup has long finished and the manager is live.
	runner func() BatchRunner

	model         func() string // configured model override; "" = the backend's cheap tier. Called fresh per Summarise
	workDir       string
	agentID       string
	maxInputChars func() int // 0 disables cap; called fresh per Summarise
}

// NewBatchSummariser builds the batch-dispatch summariser. runner is called
// fresh on each Summarise to resolve the current BatchRunner (nil if the
// agent's delegation isn't wired up yet — reported as an error, not a panic).
// model returns the configured model override ("" = ask the backend for its
// cheap tier); workDir/agentID populate the delegator.BatchRequest (empty
// values fall back to the manager's own StartOpts).
func NewBatchSummariser(runner func() BatchRunner, model func() string, workDir, agentID string, maxInputChars func() int) *BatchSummariser {
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

	model := s.model()
	result, err := br.RunBatch(ctx, delegator.BatchRequest{
		Prompt:          summaryUserMessage(content, prompt, filePath),
		SystemPrompt:    summarySystemPrompt,
		Model:           model,
		Cheap:           true,
		WorkDir:         s.workDir,
		AgentID:         s.agentID,
		OwnerSessionKey: SessionKeyFromContext(ctx),
		Purpose:         delegator.BatchPurposeSummary,
	})
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(result)

	sessionKey := SessionKeyFromContext(ctx)
	summaryLog.Infof("session=%s transport=batch model=%s input_bytes=%d output_bytes=%d",
		sessionKey, model, len(content), len(text))

	if text == "" {
		return "(empty response)", nil
	}
	return text, nil
}
