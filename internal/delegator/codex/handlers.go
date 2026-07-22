package codex

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"foci/internal/delegator"
)

// onTurnStarted signals the typing indicator.
func (b *Backend) onTurnStarted() {
	b.itemMu.Lock()
	if b.itemCache != nil {
		b.itemCache = make(map[string]itemEnvelope)
	}
	b.itemMu.Unlock()
	if b.subagents != nil {
		b.subagents.stopAll()
	}
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
	threadName := b.threadName
	b.mu.Unlock()
	if model != "" {
		model = "codex/" + model
	}
	result := &delegator.TurnResult{
		Text:       b.turnText.String(),
		ToolCalls:  b.turnTools,
		Usage:      usage,
		Model:      model,
		ThreadName: threadName,
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

	// Stash by ID for approval-request correlation.
	b.itemMu.Lock()
	if b.itemCache != nil {
		b.itemCache[item.ID] = item
	}
	b.itemMu.Unlock()

	se := b.sessionEvents.Load()
	switch item.Type {
	// Tools — feed OnToolStart so the activity indicator shows what's running.
	case "commandExecution":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "bash", item.Command)
		}
	case "fileChange":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "edit", summarizePaths(item.Changes))
		}
	case "mcpToolCall":
		if se != nil && se.OnToolStart != nil {
			name := "mcp:" + item.Server + "." + item.Tool
			se.OnToolStart(item.ID, name, truncateArgs(item.Arguments))
		}
	case "dynamicToolCall":
		if se != nil && se.OnToolStart != nil {
			name := item.Tool
			if item.Namespace != "" {
				name = item.Namespace + "." + item.Tool
			}
			se.OnToolStart(item.ID, name, truncateArgs(item.Arguments))
		}
	case "webSearch":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "web_search", item.Query)
		}
	case "imageGeneration":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "image_gen", "")
		}
	case "contextCompaction":
		if se != nil && se.OnToolStart != nil {
			se.OnToolStart(item.ID, "compact", "")
		}
	// Subagent — start polling the subagent's thread for text output.
	case "subAgentActivity":
		if item.Kind == "started" {
			if se != nil && se.OnSubagentStart != nil {
				// codex has no SendMessage-style reactivation: every subagent is a
				// single run (#1355 fields default to run 1, no prompt).
				se.OnSubagentStart(item.ID, item.AgentPath, "", 1)
			}
			if b.subagents != nil && item.AgentThreadID != "" {
				b.subagents.start(b, item.AgentThreadID, item.ID)
			}
		}
	// collabAgentToolCall — the tool-call view of a subagent spawn.
	// Carries the prompt (what was asked). Fire OnSubagentStart with the
	// prompt as the label so the client shows what the subagent is doing.
	case "collabAgentToolCall":
		if se != nil && se.OnSubagentStart != nil {
			se.OnSubagentStart(item.ID, item.Prompt, "", 1)
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
		// Only accumulate into the turn result when the phase isn't
		// "commentary" (mid-turn narration ahead of a tool call) — live
		// verified against codex app-server 0.144.5 (generate-json-schema +
		// a live turn/start->turn/steer->turn/completed probe) that
		// agentMessage items carry phase "commentary" or "final_answer".
		// A missing/empty phase (older codex, or a provider that doesn't
		// emit it — the schema's own doc calls this out) keeps the
		// pre-existing behaviour: accumulate. Commentary still reaches the
		// live view via OnText below, just excluded from the final text.
		if item.Phase != "commentary" {
			b.turnMu.Lock()
			b.turnText.WriteString(item.Text)
			b.turnMu.Unlock()
		}
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
		// Not counted in turnTools: compaction is internal bookkeeping, not
		// a user-facing tool call — counting it here skewed
		// TurnResult.ToolCalls high while collabAgentToolCall (a real
		// subagent spawn, below) skewed it low by never counting at all.
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
		if item.Kind == "interrupted" || item.Kind == "interacted" {
			// Stop polling and deliver any final text.
			if b.subagents != nil && item.AgentThreadID != "" {
				b.subagents.stop(b, item.AgentThreadID)
			}
			if item.Kind == "interrupted" && se != nil && se.OnSubagentEnd != nil {
				se.OnSubagentEnd(item.ID, 1)
			}
		}

	case "collabAgentToolCall":
		// Extract agent response messages and deliver as subagent text,
		// then close the run. Same pipeline as CC's Agent tool.
		//
		// AgentsStates is a map — Go randomizes iteration order, so ranging
		// it directly delivered multi-agent collab messages to the client
		// in a different, non-deterministic order every run. Sort by agent
		// id first for stable, reproducible delivery order.
		if se != nil {
			agentIDs := make([]string, 0, len(item.AgentsStates))
			for id := range item.AgentsStates {
				agentIDs = append(agentIDs, id)
			}
			sort.Strings(agentIDs)
			for _, id := range agentIDs {
				state := item.AgentsStates[id]
				if state.Message != "" && se.OnSubagentText != nil {
					se.OnSubagentText(item.ID, state.Message, 1) // codex has no reactivation → run 1
				}
			}
			if se.OnSubagentEnd != nil {
				se.OnSubagentEnd(item.ID, 1)
			}
		}
		// Count as a tool call like every other item type above — this was
		// previously never counted, undercounting TurnResult.ToolCalls for
		// every subagent spawn (see the contextCompaction comment above for
		// the matching overcount this pairs with).
		b.turnMu.Lock()
		b.turnTools++
		b.turnMu.Unlock()
	}
}

// itemSuccess returns true if a dynamic tool call succeeded.
func itemSuccess(item itemEnvelope) bool {
	if item.Status != "" {
		return item.Status == "completed" || item.Status == "success"
	}
	return true
}

// maxSummarizedPaths caps how many file paths summarizePaths joins into a
// single string, so a large patch (hundreds of changed files) can't blow up
// the activity indicator or, more importantly, the fileChange approval
// prompt text sent to the user's chat.
const maxSummarizedPaths = 10

// summarizePaths extracts a comma-separated list of file paths from a
// fileChange item's changes array for the activity indicator and approval
// prompt. Bounded to maxSummarizedPaths entries — an unbounded join of a
// large changeset previously produced unbounded approval text.
func summarizePaths(changes []fileChangeEntry) string {
	if len(changes) == 0 {
		return ""
	}
	shown := changes
	truncated := 0
	if len(changes) > maxSummarizedPaths {
		shown = changes[:maxSummarizedPaths]
		truncated = len(changes) - maxSummarizedPaths
	}
	parts := make([]string, 0, len(shown))
	for _, c := range shown {
		parts = append(parts, c.Path)
	}
	out := strings.Join(parts, ", ")
	if truncated > 0 {
		out += fmt.Sprintf(" (+%d more)", truncated)
	}
	return out
}

// maxTruncateArgsLen caps truncateArgs' output length in bytes.
const maxTruncateArgsLen = 200

// truncateArgs returns a truncated copy of raw JSON arguments for display.
// Truncates at a rune boundary, not a raw byte offset: raw JSON arguments
// can contain multibyte UTF-8 (non-ASCII string values), and slicing at a
// fixed byte offset can split a rune in half, producing invalid UTF-8 for
// the activity indicator.
func truncateArgs(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	s := strings.TrimSpace(string(raw))
	if len(s) <= maxTruncateArgsLen {
		return s
	}
	cut := maxTruncateArgsLen
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// onAgentMessageDelta delivers a streaming text delta for live display only.
//
// This does NOT accumulate into turnText. It used to (WriteString on every
// delta), and onItemCompleted's "agentMessage" case ALSO writes the
// completed item's full text into turnText — live-verified (codex 0.144.5)
// that a completed agentMessage's `text` is exactly the concatenation of
// its own deltas, not additional content, so every message's contribution
// to TurnResult.Text (and therefore the delivered final answer) was
// silently doubled. Found while live-verifying phase semantics for #1329
// item 6; distinct from but entangled with that fix (phase-filtering the
// doubled text would have just doubled the filtered result too).
//
// Trade-off: if the reader stops mid-message (process death / disconnect
// before item/completed fires), the interrupted-turn fallback text
// (onReaderStopped) now loses that last partial message instead of
// contributing a partial-but-doubled string — an acceptable cost in a rare
// path for correctness on every normal turn.
func (b *Backend) onAgentMessageDelta(params *agentMessageDeltaParams) {
	se := b.sessionEvents.Load()
	if se != nil && se.OnTextDelta != nil {
		se.OnTextDelta(params.Delta)
	}
}

// onTokenUsage stashes the latest usage for the current turn. Delivered
// in TurnResult.Usage when the turn completes.
func (b *Backend) onTokenUsage(params *tokenUsageParams) {
	// codex/OpenAI token semantics differ from Anthropic's: cachedInputTokens
	// is a SUBSET of inputTokens (live-verified against codex 0.144.5 rollout
	// token_count entries: input_tokens=14550 with cached_input_tokens=8960,
	// and total_tokens == input_tokens + output_tokens — the cached count is
	// already included in inputTokens, not additive). foci's downstream
	// context-fullness (internal/compaction/compact.go: input + cacheRead +
	// cacheWrite) and cost math are Anthropic-style additive, so we subtract
	// the cached portion out of InputTokens here. Reporting it in both fields
	// otherwise double-counts the cache: context occupancy inflates (premature
	// auto-compaction) and cost double-charges the cached tokens.
	inputTokens := params.TokenUsage.Last.InputTokens - params.TokenUsage.Last.CachedInputTokens
	if inputTokens < 0 {
		inputTokens = 0
	}
	u := &delegator.TurnUsage{
		InputTokens:          inputTokens,
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
	b.turnID = ""
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
