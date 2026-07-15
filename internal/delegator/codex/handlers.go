package codex

import (
	"encoding/json"

	"foci/internal/delegator"
)

// onTurnStarted signals the typing indicator.
func (b *Backend) onTurnStarted() {
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
}

// onTurnCompleted finalises the turn.
func (b *Backend) onTurnCompleted(params *turnCompletedParams) {
	b.turnMu.Lock()
	usage := b.stashedUsage
	b.turnMu.Unlock()

	b.mu.Lock()
	model := b.model
	b.mu.Unlock()

	result := &delegator.TurnResult{
		Text:      b.turnText.String(),
		ToolCalls: b.turnTools,
		Usage:     usage,
		Model:     model,
	}
	if params.Turn.Status == "failed" && params.Turn.Error != nil {
		b.lg.Warnf("turn failed: %s", params.Turn.Error.Message)
	}
	b.completeTurn(result)
}

// onItemStarted maps a Codex item/started notification to SessionEvents.
func (b *Backend) onItemStarted(params *itemStartedParams) {
	var item itemEnvelope
	if err := json.Unmarshal(params.Item, &item); err != nil {
		b.lg.Warnf("dropping malformed item in item/started: %v", err)
		return
	}

	se := b.sessionEvents.Load()
	switch item.Type {
	case "commandExecution":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "bash", "")
		}
	}
}

// onItemCompleted maps a Codex item/completed notification to SessionEvents.
func (b *Backend) onItemCompleted(params *itemCompletedParams) {
	var item itemEnvelope
	if err := json.Unmarshal(params.Item, &item); err != nil {
		b.lg.Warnf("dropping malformed item in item/completed: %v", err)
		return
	}

	se := b.sessionEvents.Load()
	switch item.Type {
	case "agentMessage":
		b.turnMu.Lock()
		b.turnText.WriteString(item.Text)
		b.turnMu.Unlock()
		if se != nil && se.OnText != nil {
			se.OnText(item.Text)
		}

	case "commandExecution":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			isError := item.Status == "failed"
			se.OnToolEnd(item.ID, "bash", "", isError)
		}

	case "reasoning":
		if se != nil && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(item.Text)
		}

	case "contextCompaction":
		b.compactMu.Lock()
		if b.compactDoneCh != nil {
			close(b.compactDoneCh)
			b.compactDoneCh = nil
		}
		b.compactMu.Unlock()
	}
}

// onAgentMessageDelta delivers a streaming text delta.
func (b *Backend) onAgentMessageDelta(params *agentMessageDeltaParams) {
	se := b.sessionEvents.Load()
	b.turnMu.Lock()
	b.turnText.WriteString(params.Delta)
	b.turnMu.Unlock()
	if se != nil && se.OnTextDelta != nil {
		se.OnTextDelta(params.Delta)
	}
}

// onTokenUsage stashes the latest usage for the current turn. Delivered
// in TurnResult.Usage when the turn completes.
func (b *Backend) onTokenUsage(params *tokenUsageParams) {
	u := &delegator.TurnUsage{
		InputTokens:          params.TokenUsage.Last.InputTokens,
		OutputTokens:         params.TokenUsage.Last.OutputTokens,
		CacheReadInputTokens: params.TokenUsage.Last.CachedInputTokens,
	}
	b.turnMu.Lock()
	b.stashedUsage = u
	b.turnMu.Unlock()

	if params.TokenUsage.ModelContextWindow > 0 {
		b.mu.Lock()
		b.contextWindow = params.TokenUsage.ModelContextWindow
		b.mu.Unlock()
	}
}

// onServerRequestResolved confirms an approval was answered. Fires
// onPromptsCleared if no more pending approvals remain.
func (b *Backend) onServerRequestResolved(_ *serverRequestResolvedParams) {
	b.permMu.Lock()
	isEmpty := len(b.pendingPerms) == 0
	b.permMu.Unlock()
	if isEmpty && b.onPromptsCleared != nil {
		b.onPromptsCleared()
	}
}

// onReasoningDelta delivers streaming raw reasoning text.
func (b *Backend) onReasoningDelta(params *reasoningDeltaParams) {
	se := b.sessionEvents.Load()
	if se != nil && se.OnThinkingDelta != nil {
		se.OnThinkingDelta(params.Delta)
	}
}

// onReasoningSummaryDelta delivers streaming reasoning summary text.
func (b *Backend) onReasoningSummaryDelta(params *reasoningSummaryDeltaParams) {
	se := b.sessionEvents.Load()
	if se != nil && se.OnThinkingDelta != nil {
		se.OnThinkingDelta(params.Delta)
	}
}

// onConfigWarning logs recoverable configuration problems surfaced by the
// app-server and fires the onWarning hook so they reach the user's chat.
func (b *Backend) onConfigWarning(params *configWarningParams) {
	msg := params.Summary
	if params.Details != "" {
		msg += ": " + params.Details
	}
	if params.Path != "" {
		msg += " (" + params.Path + ")"
	}
	b.lg.Infof("config warning: %s", msg)
	b.fireWarning(msg)
}

// completeTurn fires the OnTurnComplete callback and clears turn state.
func (b *Backend) completeTurn(result *delegator.TurnResult) {
	b.turnMu.Lock()
	turn := b.turnEvents
	b.turnEvents = nil
	b.turnActive = false
	ch := b.turnResultCh
	b.turnResultCh = nil
	b.turnText.Reset()
	b.turnTools = 0
	b.stashedUsage = nil
	b.turnMu.Unlock()

	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}
	if ch != nil {
		select {
		case ch <- result:
		default:
		}
	}
}
