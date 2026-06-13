package ccstream

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"foci/internal/delegator"
	"foci/internal/log"
)

// OnAssistant handles assistant messages from CC's stdout.
//
// Sub-agent messages (ParentToolUseID != nil) are filtered out of the
// turn-state updates and handler callbacks below — sub-agents run their own
// turn via the Agent tool, and their text / tool_use blocks belong to the
// sub-agent's transcript rather than the parent turn the caller is
// observing. Without this guard, sub-agent text would fire OnText onto the
// parent's StreamingSink (rendering nested text twice) and sub-agent
// tool_use blocks would fire OnToolStart onto the parent tracker. Model /
// usage tracking is already gated on isTopLevel to protect the primary
// model name from subagent haiku overrides.
func (b *Backend) OnAssistant(msg *AssistantMessage) {
	b.touchActivity()
	isTopLevel := msg.ParentToolUseID == nil

	// Block-type breakdown for diagnostics — distinguishes "model
	// produced text but it didn't reach delivery" from "model produced
	// no text block at all" when investigating delivery gaps.
	if isTopLevel {
		var textBlocks, toolUseBlocks, thinkingBlocks, totalTextBytes int
		for _, block := range msg.Message.Content {
			switch block.Type {
			case "text":
				textBlocks++
				totalTextBytes += len(block.Text)
			case "tool_use":
				toolUseBlocks++
			case "thinking":
				thinkingBlocks++
			}
		}
		stopReason := ""
		if msg.Message.StopReason != nil {
			stopReason = *msg.Message.StopReason
		}
		log.Debugf("ccstream", "OnAssistant: text_blocks=%d tool_use_blocks=%d thinking_blocks=%d text_bytes=%d stop_reason=%s",
			textBlocks, toolUseBlocks, thinkingBlocks, totalTextBytes, stopReason)
	}

	b.mu.Lock()
	if isTopLevel && msg.Message.Model != "" {
		b.lastModel = msg.Message.Model
	}
	if isTopLevel {
		u := msg.Message.Usage
		b.lastUsage = &u
	}
	b.mu.Unlock()

	// Delivery callbacks come from the session-scoped SessionEvents — never
	// nil after first AttachSessionEvents, so text/tool blocks always have
	// somewhere to go regardless of per-turn TurnEvents state. This is what
	// kills the "text block dropped: handler nil" failure mode at backend
	// layer; see TODO #747.
	se := b.sessionEvents.Load()

	if !isTopLevel {
		// Surface sub-agent text as blockquoted intermediate replies so
		// the user can follow sub-agent progress. Tool_use blocks are not
		// forwarded — the parent tracker owns tool visibility.
		//
		// Route via OnSubagentText (carrying the parent tool_use id as the
		// group key) when the consumer supports it — that lets the platform
		// attach a rolling "Hide this" control and delete the group on demand.
		// Fall back to OnText for consumers without subagent support.
		groupKey := ""
		if msg.ParentToolUseID != nil {
			groupKey = *msg.ParentToolUseID
		}
		if se != nil {
			for _, block := range msg.Message.Content {
				if block.Type != "text" || block.Text == "" {
					continue
				}
				switch {
				case se.OnSubagentText != nil:
					se.OnSubagentText(groupKey, blockquote(block.Text))
				case se.OnText != nil:
					se.OnText(blockquote(block.Text))
				}
			}
		}
		// Keep typing indicator alive during sub-agent work.
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
		return
	}

	// Separate this message's text from any text already accumulated by PRIOR
	// assistant messages in this turn (segments split by tool calls) with a
	// blank line — otherwise pre-tool-call narration glues onto the next
	// segment (e.g. "...correctly.Καλημέρα"). Text blocks WITHIN a single
	// message are still concatenated directly: the model may split one sentence
	// across blocks ("Hello " + "world!"). See TODO #819.
	b.turnMu.Lock()
	needSep := b.turnText.Len() > 0
	b.turnMu.Unlock()

	for _, block := range msg.Message.Content {
		switch block.Type {
		case "text":
			b.turnMu.Lock()
			if block.Text != "" {
				if needSep {
					b.turnText.WriteString("\n\n")
					needSep = false
				}
				b.turnText.WriteString(block.Text)
			}
			b.turnMu.Unlock()

			if se != nil && se.OnText != nil {
				se.OnText(block.Text)
			}

		case "tool_use":
			b.turnMu.Lock()
			b.turnTools++
			b.turnMu.Unlock()

			if se != nil && se.OnToolStart != nil {
				inputStr := string(block.Input)
				se.OnToolStart(block.ID, block.Name, inputStr)
			}

			// Track Agent tool calls for status reporting (same as tmux backend).
			if block.Name == "Agent" {
				desc := delegator.ExtractAgentDescription(block.Input)
				b.agents.Add(block.ID, desc)
			}

		case "thinking":
			// Thinking blocks are informational; optionally log.
		}
	}

	// Restart typing indicator if the turn hasn't ended.
	if msg.Message.StopReason == nil || *msg.Message.StopReason != "end_turn" {
		if b.typingFunc != nil {
			b.typingFunc(true)
		}
	}
}

// OnResult handles the result message signalling turn completion.
func (b *Backend) OnResult(msg *ResultMessage) {
	b.touchActivity()

	// Capture turn state. TurnEvents clearing is deferred — the pre-answer
	// gate path needs OnTurnComplete alive across rounds. Normal path
	// clears turnEvents/turnActive below.
	b.turnMu.Lock()
	turn := b.turnEvents
	// Claim the turn for this completion immediately: capture turn into a local
	// and clear b.turnEvents in the SAME critical section that sets
	// `completing`. This narrows the window in which finalizeExit (which reads
	// b.turnEvents) could capture the same TurnEvents and fire a second
	// OnTurnComplete (P1-8). The re-arm paths re-install b.turnEvents via
	// beginTurn for the next round, and the current round fires completion via
	// the local `turn`, so early-clearing is safe. The completion itself is
	// also guarded by a sync.Once at the agent layer as the hard backstop.
	b.turnEvents = nil
	resultCh := b.turnResultCh
	turnText := b.turnText.String()
	turnTools := b.turnTools
	// Capture (and close) any open shadow-reply window: this result IS that
	// shadow reply (or the round-1 result of a fresh fold). Used only for
	// xtra:ccstream instrumentation. A chained re-arm below re-opens the window.
	wasAwaitingShadow := b.awaitingShadow
	shadowReArmAt := b.reArmAt
	b.awaitingShadow = false
	// A real result arrived: claim this turn against the re-arm watchdog and
	// disarm it. If we go on to re-arm below, beginTurn resets `completing` and
	// a fresh watchdog is armed for the new generation (#813).
	b.completing = true
	if b.watchdog != nil {
		b.watchdog.Stop()
		b.watchdog = nil
	}
	b.turnMu.Unlock()

	// Build TurnResult. Prefer turnText (accumulated from all assistant
	// messages in the turn) over msg.Result (which only contains the last
	// segment). Multi-segment turns (text → tool → text) need the full text.
	text := turnText
	if text == "" {
		text = msg.Result
	}

	// Determine model from lastModel (set by OnAssistant, filtered to top-level
	// messages only — subagent models are excluded). Use per-call usage from
	// the last assistant message (not the result's accumulated total) — this
	// matches what the tmux watcher reports and gives compaction the actual
	// context window fill, not a sum of all calls.
	b.mu.Lock()
	resultModel := b.lastModel
	lastUsage := b.lastUsage
	b.lastUsage = nil // reset for next turn
	b.mu.Unlock()

	// Pick context window from ModelUsage deterministically: prefer the
	// entry matching resultModel (the primary model from assistant messages);
	// otherwise take the largest context window to avoid spurious compaction
	// from subagent models (e.g. haiku) winning the random map iteration.
	if usage, ok := msg.ModelUsage[resultModel]; ok {
		b.mu.Lock()
		b.contextWindow = usage.ContextWindow
		b.mu.Unlock()
	} else {
		var bestCW int
		for _, usage := range msg.ModelUsage {
			if usage.ContextWindow > bestCW {
				bestCW = usage.ContextWindow
			}
		}
		if bestCW > 0 {
			b.mu.Lock()
			b.contextWindow = bestCW
			b.mu.Unlock()
		}
	}

	// Prefer per-call usage from last assistant message; fall back to
	// result usage (which is cumulative) if no assistant messages seen.
	var turnUsage *delegator.TurnUsage
	if lastUsage != nil {
		turnUsage = &delegator.TurnUsage{
			InputTokens:              lastUsage.InputTokens,
			OutputTokens:             lastUsage.OutputTokens,
			CacheCreationInputTokens: lastUsage.CacheCreationInputTokens,
			CacheReadInputTokens:     lastUsage.CacheReadInputTokens,
		}
	} else {
		turnUsage = &delegator.TurnUsage{
			InputTokens:              msg.Usage.InputTokens,
			OutputTokens:             msg.Usage.OutputTokens,
			CacheCreationInputTokens: msg.Usage.CacheCreationInputTokens,
			CacheReadInputTokens:     msg.Usage.CacheReadInputTokens,
		}
	}

	result := &delegator.TurnResult{
		Text:      text,
		Model:     resultModel,
		ToolCalls: turnTools,
		Usage:     turnUsage,
	}

	// Pre-answer nudge gate: give the caller a chance to re-dispatch this
	// turn with a verification prompt before finalising. When the func
	// returns a non-empty follow-up, the result is swallowed, beginTurn
	// is called again with the same TurnEvents, and the follow-up is sent
	// as a new user message — explicitly starting a fresh CC ask(). The
	// next OnResult delivers the revised answer as the authoritative
	// outcome. The caller must stop returning a follow-up after the first
	// fire to break the loop (guaranteed by the scheduler's internal
	// state — CheckPreAnswer returns the same text every call but the
	// turn_delegated closure tracks "fired" locally). This is distinct
	// from the mid-turn-drain path used by SourceUser/Steer/post-tool
	// nudges: pre-answer needs a fresh ask() because it's a verification
	// re-prompt, not a fold-in.
	//
	// See docs/WIRING.md → "Shadow-turn re-arm + watchdog (#813)" for the
	// full re-arm/heldResult/watchdog map (this is the pre-answer caller).
	if turn != nil && turn.PreAnswerNudgeFunc != nil {
		if followUp := turn.PreAnswerNudgeFunc(result); followUp != "" {
			if b.reArmForContinuation(turn, followUp) {
				b.logger().Extra("steer_shadow event=preanswer_redispatch followup_len=%d round1_output=%d round1_textlen=%d",
					len(followUp), result.Usage.OutputTokens, len(result.Text))
				return
			}
			// Re-dispatch failed; fall through to the normal completion
			// path so the first-round result is still delivered.
		}
	}

	// Folded-steer / follow-up re-arm gate. A mid-turn steer or in-flight
	// user follow-up written to CC's stdin makes CC emit THIS result and then
	// produce the real reply as a SEPARATE result. Re-arm the turn (same
	// TurnEvents, same delivering sink, refcount stays held) so that reply has
	// a live turn to land in instead of running as an untracked shadow turn
	// that a colliding inject can lose (#813). Ordered AFTER the pre-answer
	// gate (a verification re-prompt wins if both somehow apply) and BEFORE the
	// normal clear. A counter: consume exactly one pending fold per result.
	//
	// See docs/WIRING.md → "Shadow-turn re-arm + watchdog (#813)" for the
	// full re-arm/heldResult/watchdog map (this is the steer re-arm caller).
	b.turnMu.Lock()
	reArm := b.pendingSteer > 0
	if reArm {
		b.pendingSteer--
	}
	b.turnMu.Unlock()
	if reArm && turn != nil {
		// Stash this result BEFORE re-arming: reArmForContinuation's beginTurn
		// resets turnText, so if no shadow reply ever arrives the watchdog
		// delivers this (in the no-second-OnResult fold mode, it IS the answer).
		b.turnMu.Lock()
		b.heldResult = result
		b.reArmDepth++
		depth := b.reArmDepth
		b.turnMu.Unlock()
		b.logger().Debugf("OnResult: re-arming turn for folded steer/follow-up shadow reply (#813)")
		b.reArmForContinuation(turn, "")
		b.armReArmWatchdog()
		// depth=1 is a first fold (round_* is the round-1 result); depth>1 is a
		// chained fold (the prior shadow reply itself folded — round_* is the
		// round-N result, not round 1).
		b.logger().Extra("steer_shadow event=rearm depth=%d round_output=%d round_textlen=%d watchdog_bound=%s outstanding_prompts=%d",
			depth, result.Usage.OutputTokens, len(result.Text), b.watchdogBound(), b.outstandingPrompts())
		return
	}

	// Normal turn completion — clear TurnEvents. SessionEvents stay live for
	// the rest of the session so any post-turn text (e.g. CC running a
	// follow-up ask() from a folded steer) still delivers cleanly.
	b.turnMu.Lock()
	hadTurn := b.turnEvents != nil
	chainDepth := b.reArmDepth
	b.turnEvents = nil
	b.turnActive = false
	b.pendingSteer = 0 // turn truly ended; drop any unconsumed fold marks
	b.heldResult = nil // no longer need the stashed re-arm result
	b.reArmDepth = 0   // chain terminated by a real completion
	b.turnMu.Unlock()
	b.logger().Debugf("OnResult: turn cleared (had_turn_events=%v)", hadTurn)
	if wasAwaitingShadow {
		outcome := "delivered"
		if text == "" {
			outcome = "empty"
		}
		b.logger().Extra("steer_shadow event=shadow_result outcome=%s depth=%d output=%d textlen=%d rearm_to_result=%s outstanding_prompts=%d",
			outcome, chainDepth, turnUsage.OutputTokens, len(text), time.Since(shadowReArmAt).Round(time.Millisecond), b.outstandingPrompts())
	}

	// Clear any agents still tracked (safety net — task_notification should
	// have already removed them individually during the turn).
	b.agents.ClearAll()

	// Fire OnTurnComplete OUTSIDE any lock.
	if turn != nil && turn.OnTurnComplete != nil {
		turn.OnTurnComplete(result)
	}

	// Stop typing indicator.
	if b.typingFunc != nil {
		b.typingFunc(false)
	}

	// Signal WaitForTurn (non-blocking).
	if resultCh != nil {
		select {
		case resultCh <- msg:
		default:
		}
	}
}

// OnSystem handles system messages (init, status, compact_boundary, etc.).
func (b *Backend) OnSystem(subtype string, raw json.RawMessage) {
	b.touchActivity()
	switch subtype {
	case "init":
		var init InitMessage
		if err := json.Unmarshal(raw, &init); err != nil {
			return
		}
		b.mu.Lock()
		b.sessionID = init.SessionID
		b.initMsg = &init
		b.lastModel = init.Model
		b.mu.Unlock()
		b.readyOnce.Do(func() { close(b.readyCh) })
		if b.onSessionReady != nil {
			b.onSessionReady(init.SessionID)
		}

	case "status":
		var status StatusMessage
		if err := json.Unmarshal(raw, &status); err != nil {
			return
		}
		if status.Status != nil && *status.Status == "compacting" {
			if b.onCompactionStart != nil {
				b.onCompactionStart()
			}
			// Signal any armed compaction start waiter (one-shot).
			b.turnMu.Lock()
			sch := b.compactStartCh
			b.compactStartCh = nil
			b.turnMu.Unlock()
			if sch != nil {
				select {
				case sch <- struct{}{}:
				default:
				}
			}
		}

	case "compact_boundary":
		var cb CompactBoundaryMessage
		if err := json.Unmarshal(raw, &cb); err != nil {
			return
		}
		if b.onCompactionDone != nil {
			b.onCompactionDone(cb.CompactMetadata.PreTokens)
		}
		// Signal any armed compaction waiter (one-shot; clear after firing).
		b.turnMu.Lock()
		ch := b.compactDoneCh
		b.compactDoneCh = nil
		b.turnMu.Unlock()
		if ch != nil {
			select {
			case ch <- struct{}{}:
			default:
			}
		}

	case "session_state_changed":
		var ss SessionStateMessage
		_ = json.Unmarshal(raw, &ss)

	case "task_started", "task_progress", "task_notification":
		var task TaskEvent
		if err := json.Unmarshal(raw, &task); err != nil {
			return
		}
		switch subtype {
		case "task_notification":
			if task.Status == "completed" {
				// Remove one pending agent. If the tracker had nothing
				// (e.g. tool_use detection missed it), fire a standalone
				// notification as fallback.
				if !b.agents.RemoveOne() && b.agents.OnStatus != nil {
					b.agents.OnStatus(fmt.Sprintf("✅ Task complete: %s", task.Summary))
				}
			}
		}

	case "api_retry":
		// CC handles its own API retries internally; we parse the message
		// for symmetry with the protocol but do not surface it to the user.
		// The turnevent.RetryNotice / RetrySuccess UI is for the API tool
		// loop's own retries, which don't apply when CC owns inference.
		var retry APIRetryMessage
		if err := json.Unmarshal(raw, &retry); err != nil {
			return
		}
		_ = retry

	case "hook_response":
		// PostToolUse / PostToolUseFailure hook completions. Parsed and
		// dispatched to the sessions SessionEvents.OnToolEnd via the
		// helper defined in hooks.go.
		b.handleHookResponse(raw)

	case "elicitation_complete":
		// CC re-broadcasts an MCP server's elicitation_complete notification
		// when a URL-mode flow was completed externally. Match by
		// elicitation_id and auto-accept so the user doesn't have to click
		// Done after already finishing in the browser.
		var done ElicitationCompleteMessage
		if err := json.Unmarshal(raw, &done); err != nil {
			return
		}
		b.OnElicitationComplete(&done)
	}
}

// OnPermissionRequest handles can_use_tool control requests from CC.
// Dispatches to tool-specific handlers (e.g. AskUserQuestion) or the
// standard permission prompt flow.
func (b *Backend) OnPermissionRequest(msg *PermissionRequest) {
	b.touchActivity()
	b.handleToolRequest(msg)
}

// OnControlResponse handles responses to our control requests (e.g. initialize,
// get_context_usage). Routes to pending waiters by request_id.
//
// For fresh sessions (no --resume), CC responds to the initialize control
// request with a control_response rather than emitting a system/init message.
// When we detect the initialize response, we close readyCh so WaitReady
// unblocks.
func (b *Backend) OnControlResponse(raw json.RawMessage) {
	b.touchActivity()
	var env controlResponseInbound
	if err := json.Unmarshal(raw, &env); err != nil {
		log.Debugf("ccstream", "unmarshal control_response: %v", err)
		return
	}
	reqID := env.Response.RequestID
	if reqID == "" {
		return
	}

	// Check if this is the response to our initialize request.
	b.mu.Lock()
	isInit := b.initReqID != "" && reqID == b.initReqID
	if isInit {
		b.initReqID = "" // consume — only match once
	}
	b.mu.Unlock()
	if isInit {
		b.readyOnce.Do(func() { close(b.readyCh) })
	}

	b.pendingControlMu.Lock()
	ch, ok := b.pendingControls[reqID]
	if ok {
		delete(b.pendingControls, reqID)
	}
	b.pendingControlMu.Unlock()
	if ok {
		select {
		case ch <- raw:
		default:
		}
	}
}

// OnControlCancelRequest handles CC cancelling a pending control request.
func (b *Backend) OnControlCancelRequest(reqID string) {
	b.touchActivity()
	b.handleControlCancel(reqID)
}

// OnKeepAlive handles heartbeat events. Touches activity so the idle/timeout
// tracker sees the stream as alive during periods where CC is blocked (e.g.
// waiting for a permission prompt response) and not emitting work events.
//
// NOTE: As of CC 1.x, keep_alive frames are only sent on WebSocket transports
// (remote control sessions). In --pipe mode (stdin/stdout, which foci uses),
// CC never sends keep_alive — so this handler is effectively dead code.
// The idle tracker must be kept alive by other means (e.g. touchActivity on
// permission request arrival). See also runKeepAlive which sends keep_alive
// TO CC (also a no-op: CC silently ignores them in pipe mode).
func (b *Backend) OnKeepAlive() {
	b.touchActivity()
}

// OnRateLimit handles rate limit events from CC's stdout.
func (b *Backend) OnRateLimit(msg *RateLimitEvent) {
	b.touchActivity()
	if b.rateLimitState != nil {
		b.rateLimitState.Update(&msg.RateLimitInfo)
	}
}

// OnToolProgress handles heartbeats during long-running tool execution.
func (b *Backend) OnToolProgress(msg *ToolProgressMessage) {
	b.touchActivity()
	// Keep typing indicator alive during tool execution.
	if b.typingFunc != nil {
		b.typingFunc(true)
	}
}

// OnStreamEvent handles token-level streaming events. CC wraps Anthropic
// SDK stream parts in these envelopes (services/api/claude.ts:2300), so the
// event payload is a verbatim SDK `content_block_delta` with subtypes like
// `text_delta` and `thinking_delta` that we extract separately.
//
// Sub-agent stream events (ParentToolUseID != nil) are filtered out, matching
// the guard in OnAssistant. Sub-agent text is delivered as complete blocks
// (blockquoted) via OnAssistant instead. Without this filter, sub-agent
// deltas leak into the parent turn's StreamWriter — accumulating text that
// is never Finish()ed by OnReply, which corrupts the parent's stream message
// and silently discards the parent's reply text.
func (b *Backend) OnStreamEvent(raw json.RawMessage) {
	b.touchActivity()
	var env struct {
		ParentToolUseID *string `json:"parent_tool_use_id,omitempty"`
		Event           struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		} `json:"event"`
	}
	if json.Unmarshal(raw, &env) != nil || env.Event.Type != "content_block_delta" {
		return
	}
	if env.ParentToolUseID != nil {
		return
	}
	// Deltas route through SessionEvents so they survive across stacked
	// turns / post-OnResult emission, same reasoning as OnAssistant text.
	se := b.sessionEvents.Load()
	if se == nil {
		return
	}
	switch env.Event.Delta.Type {
	case "text_delta":
		if env.Event.Delta.Text != "" && se.OnTextDelta != nil {
			se.OnTextDelta(env.Event.Delta.Text)
		}
	case "thinking_delta":
		if env.Event.Delta.Thinking != "" && se.OnThinkingDelta != nil {
			se.OnThinkingDelta(env.Event.Delta.Thinking)
		}
	}
}

// blockquote prefixes every line with "> " for markdown blockquote rendering.
func blockquote(text string) string {
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = "> " + line
	}
	return strings.Join(lines, "\n")
}
