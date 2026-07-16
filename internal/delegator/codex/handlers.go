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
	if model != "" {
		model = "codex/" + model
	}
	result := &delegator.TurnResult{
		Text:       b.turnText.String(),
		ToolCalls:  b.turnTools,
		Usage:      usage,
		Model:      model,
		ThreadName: b.threadName,
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
	// Tools — feed OnToolStart so the activity indicator shows what's running.
	case "commandExecution":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "bash", item.Command)
		}
	case "fileChange":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "edit", "")
		}
	case "mcpToolCall":
		if se != nil && se.OnToolStart != nil {
			name := "mcp:" + item.Server + "." + item.Tool
			se.OnToolStart(item.ID, name, "")
		}
	case "dynamicToolCall":
		if se != nil && se.OnToolStart != nil {
			name := item.Tool
			if item.Namespace != "" {
				name = item.Namespace + "." + item.Tool
			}
			se.OnToolStart(item.ID, name, "")
		}
	case "webSearch":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "web_search", "")
		}
	case "imageGeneration":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "image_gen", "")
		}
	case "contextCompaction":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "compact", "")
		}
	// Subagent — feed OnSubagentStart for the activity indicator's
	// "subagents" kind.
	case "subAgentActivity":
		if item.Kind == "started" && se != nil && se.OnSubagentStart != nil {
			se.OnSubagentStart(item.ID, item.AgentPath)
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

	case "fileChange":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "edit", "", item.Status == "failed")
		}

	case "mcpToolCall":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			name := "mcp:" + item.Server + "." + item.Tool
			se.OnToolEnd(item.ID, name, "", item.Status == "failed")
		}

	case "dynamicToolCall":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			name := item.Tool
			if item.Namespace != "" {
				name = item.Namespace + "." + item.Tool
			}
			se.OnToolEnd(item.ID, name, "", !itemSuccess(item))
		}

	case "webSearch":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "web_search", "", false)
		}

	case "imageGeneration":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "image_gen", "", item.Status == "failed")
		}

	case "reasoning":
		if se != nil && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(item.Text)
		}

	case "contextCompaction":
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
		b.compactMu.Lock()
		if b.compactDoneCh != nil {
			close(b.compactDoneCh)
			b.compactDoneCh = nil
		}
		b.compactMu.Unlock()
		if se != nil && se.OnToolEnd != nil {
			se.OnToolEnd(item.ID, "compact", "", false)
		}

	case "subAgentActivity":
		if item.Kind == "interrupted" && se != nil && se.OnSubagentEnd != nil {
			se.OnSubagentEnd(item.ID)
		}
	}
}

// itemSuccess returns true if a dynamic tool call succeeded.
func itemSuccess(item itemEnvelope) bool {
	if item.Status != "" {
		return item.Status == "completed" || item.Status == "success"
	}
	return true
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
