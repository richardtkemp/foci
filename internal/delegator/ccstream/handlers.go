package ccstream

import (
	"encoding/json"

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
					// Raw text; the label rides SubagentStart, and blockquote is a
					// per-platform choice applied in the renderer, not here.
					se.OnSubagentText(groupKey, block.Text)
				case se.OnText != nil:
					se.OnText(block.Text)
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
	turnActive := b.turnActive
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
				if !turnActive {
					// Autonomous run: this text streams to the chat via the
					// session sink's late-delivery fallback, so onSessionIdle
					// must not re-deliver the stashed result (#1063).
					b.autonomousStreamed = true
				}
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

			// Track background work for status reporting AND the pending-work gate
			// (spec §4). Agent-tool subagents and run_in_background Bash both
			// outlive their turn and drive a task_notification / autonomous run on
			// completion, so both must count toward Pending(). A synchronous Bash
			// completes inside the turn and is not tracked.
			if block.Name == "Agent" {
				desc := delegator.ExtractAgentDescription(block.Input)
				b.agents.Add(block.ID, desc)
			} else if block.Name == "Bash" && delegator.ExtractBashBackground(block.Input) {
				b.agents.Add(block.ID, "background command")
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

// OnResult handles a result message. Under the idle-keyed lifecycle a result
// is NOT the turn boundary — it is one internal ask cycle's accounting. CC
// mints 0, 1 or N results per logical turn (a "now" steer aborts the current
// ask and adds one; a steer landing mid-tool folds and adds none; results are
// withheld while background agents run), so the result is stashed (latest
// wins; output tokens accumulate across cycles) and the turn completes when
// CC's session_state_changed:idle arrives — see onSessionIdle.
//
// Legacy fallback: when CC has emitted no session-state events this session
// (env unset, older binary), complete on the result as the pre-idle design
// did. See docs/WIRING.md → "Idle-keyed turn completion".
func (b *Backend) OnResult(msg *ResultMessage) {
	b.touchActivity()

	b.turnMu.Lock()
	turnActive := b.turnActive
	turnText := b.turnText.String()
	turnTools := b.turnTools
	b.turnMu.Unlock()

	// Build TurnResult. Prefer turnText (accumulated from all assistant
	// messages in the turn) over msg.Result (which only contains the last
	// segment). Multi-segment turns (text → tool → text) need the full text.
	text := turnText
	if text == "" {
		text = msg.Result
	}

	// Detect a 401 auth failure surfaced as an error result and trigger
	// automated re-login (#843). Firing here and on the subprocess exit path is
	// safe — the re-login gate single-flights.
	if msg.IsError && isAuthFailure(text) {
		b.fireAuthFailure(text)
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

	// Input/cache come from the last assistant message — the FINAL call's
	// context fill, which compaction needs (not a sum of all calls). Fall back
	// to the result's accumulated usage if no assistant messages were seen.
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

	// OUTPUT tokens must NOT be trusted from lastUsage: the last assistant
	// message's usage in the live stream is an early/partial snapshot (often
	// output_tokens≈1) that is never refreshed to the final count before this
	// result arrives, so lastUsage.OutputTokens massively undercounts a
	// substantive reply — a ~2000-token answer logged as output=4 (#721),
	// undercounting api.db delegated-turn cost. The result's per-model
	// accounting (msg.ModelUsage[resultModel]) is CC's authoritative end-of-turn
	// total for the primary model — subagent models are separate keys, so they
	// stay excluded from the primary's cost. On a key miss, fall back to the
	// result's accumulated total (msg.Usage, all models). Apply as a floor so
	// it can only correct an undercount, never regress a good value.
	authoritativeOutput := msg.Usage.OutputTokens
	if mu, ok := msg.ModelUsage[resultModel]; ok {
		authoritativeOutput = mu.OutputTokens
	}
	if authoritativeOutput > turnUsage.OutputTokens {
		turnUsage.OutputTokens = authoritativeOutput
	}

	result := &delegator.TurnResult{
		Text:      text,
		Model:     resultModel,
		ToolCalls: turnTools,
		Usage:     turnUsage,
	}

	// Stash this cycle's result; the turn total for output tokens is the sum
	// across cycles (each result's usage is per-ask-cycle, probe-verified),
	// while text (turnText spans the whole turn), tool count, model and
	// input/cache (the FINAL cycle's context fill — what compaction needs)
	// are latest-wins. A fresh result also satisfies any pre-answer
	// re-dispatch that was holding the turn open at idle.
	b.turnMu.Lock()
	b.turnCalls++
	cycle := b.turnCalls
	b.turnOutputTokens += result.Usage.OutputTokens
	result.Usage.OutputTokens = b.turnOutputTokens
	b.stashedResult = result
	b.stashedResultMsg = msg
	b.redispatchInFlight = false
	stateSeen := b.stateEventsSeen
	b.turnMu.Unlock()

	b.logger().Debugf("OnResult: stashed ask-cycle result (turn_active=%v cycle=%d textlen=%d out_total=%d)",
		turnActive, cycle, len(text), result.Usage.OutputTokens)
	b.logger().Debugf("turn_lifecycle event=result_stash cycle=%d turn_active=%v subtype=%s textlen=%d out_total=%d",
		cycle, turnActive, msg.Subtype, len(text), result.Usage.OutputTokens)

	if !turnActive {
		// Autonomous turn (no foci turn open — e.g. a background-agent or Bash
		// completion triggers a task-notification run). Its text already
		// delivered via the always-live SessionEvents; nothing to complete.
		return
	}

	if !stateSeen {
		// CC is not emitting session-state events — no idle will ever come.
		// Complete on the result, the pre-idle-keyed behaviour (including the
		// pre-answer verification gate, which normally runs at idle).
		b.turnMu.Lock()
		warned := b.fallbackWarned
		b.fallbackWarned = true
		turn := b.turnEvents
		b.turnMu.Unlock()
		if !warned {
			b.logger().Warnf("no session_state_changed events from CC (CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS unset or unsupported); falling back to complete-on-result — steers folded mid-turn may complete early")
		}
		if b.tryPreAnswerRedispatch(turn, result) {
			return
		}
		b.completeTurn("result-fallback")
	}
}

// OnSystem handles system messages (init, status, compact_boundary, etc.).
func (b *Backend) OnSystem(subtype string, raw json.RawMessage) {
	b.touchActivity()
	switch subtype {
	case "init":
		var init InitMessage
		if err := json.Unmarshal(raw, &init); err != nil {
			log.Warnf("ccstream", "drop init message (unmarshal failed): %v — WaitReady will stall", err)
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
			log.Warnf("ccstream", "drop status message (unmarshal failed): %v — compaction-start waiter may stall", err)
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
			log.Warnf("ccstream", "drop compact_boundary message (unmarshal failed): %v — compaction-done waiter may stall", err)
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
		// CC's authoritative run-loop boundary (opt-in via
		// CLAUDE_CODE_EMIT_SESSION_STATE_EVENTS=1, set at launch). `running`
		// fires at run() entry; `idle` fires at run() exit, AFTER the held-back
		// result flush — it is the turn-completion signal. See OnResult and
		// docs/WIRING.md → "Idle-keyed turn completion".
		var ss SessionStateMessage
		if err := json.Unmarshal(raw, &ss); err != nil {
			log.Warnf("ccstream", "drop session_state_changed (unmarshal failed): %v — turn completion may fall to the orchestrator timeout", err)
			return
		}
		b.turnMu.Lock()
		b.stateEventsSeen = true
		turnActive := b.turnActive
		var fireAutonomousStart func()
		if ss.State == "running" && !turnActive {
			fireAutonomousStart = b.setAutonomousActiveLocked(true)
			b.autonomousStreamed = false // fresh run: no text streamed yet
		}
		autonomous := b.autonomousActive
		b.turnMu.Unlock()
		if fireAutonomousStart != nil {
			fireAutonomousStart()
		}
		b.logger().Debugf("turn_lifecycle event=session_state state=%s turn_active=%v autonomous=%v", ss.State, turnActive, autonomous)
		if ss.State == "idle" {
			b.onSessionIdle()
		}

	case "task_started", "task_progress", "task_notification":
		var task TaskEvent
		if err := json.Unmarshal(raw, &task); err != nil {
			log.Warnf("ccstream", "drop %s message (unmarshal failed): %v — task tracker may not clear", subtype, err)
			return
		}
		switch subtype {
		case "task_notification":
			if task.Status == "completed" {
				// Remove one pending subagent. If the tracker had nothing
				// (e.g. tool_use detection missed it), the resolved state is
				// already "no subagents running" — signal that with an empty
				// detail so any stale indicator clears.
				if !b.agents.RemoveOne() && b.agents.OnStatus != nil {
					b.agents.OnStatus("")
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
			log.Warnf("ccstream", "drop elicitation_complete message (unmarshal failed): %v — URL elicitation will not auto-resolve", err)
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
func (b *Backend) OnRateLimit(_ *RateLimitEvent) {
	b.touchActivity()
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
		// Fire on event presence, not content: this model streams thinking with
		// empty plaintext (only the signature), so gating on non-empty text would
		// never light the indicator. renderer.OnThinkingDelta no-ops on empty.
		if se.OnThinkingDelta != nil {
			se.OnThinkingDelta(env.Event.Delta.Thinking)
		}
	}
}
