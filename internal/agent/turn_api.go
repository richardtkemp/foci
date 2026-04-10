package agent

import (
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/config"
	"foci/internal/log"
	"foci/internal/provider"
)

// ---------------------------------------------------------------------------
// APITransport — TurnContract implementations for the API tool-loop path.
// Each method is a direct extraction from HandleMessageWithAttachments.
// These exist but are NOT called until Stage 6 (the switchover).
// ---------------------------------------------------------------------------

// --- Phase 1: Pre-lock gates and registration ---

// RateLimitGate checks the per-endpoint rate limit gate.
// Extracted from agent.go:286-301.
func (t *APITransport) RateLimitGate(ts *TurnState) error {
	a := t.agent
	sm := a.getSessionMeta(ts.SessionKey)
	a.metaMu.Lock()
	endpoint := sm.modelEndpoint
	if endpoint == "" {
		endpoint = a.Endpoint
	}
	a.metaMu.Unlock()

	gate := a.getOrCreateRateLimitGate(endpoint)
	if limited, until := gate.IsLimited(); limited {
		gate.Enqueue(ts.SessionKey, ts.Texts[0], ts.Trigger)
		a.logger().Infof("rate limit gate (%s): queued message for session=%s trigger=%s (resets %s)",
			endpoint, ts.SessionKey, ts.Trigger, until.Format(time.Kitchen))
		return &RateLimitedError{Until: until}
	}
	return nil
}

// AcquireTurnLock acquires the per-session turn serialization lock.
// Extracted from agent.go:309-316.
func (t *APITransport) AcquireTurnLock(ts *TurnState) func() {
	a := t.agent
	sessionLock := a.turnLock(ts.SessionKey)
	a.logger().Debugf("turn_lock_wait session=%s trigger=%s", ts.SessionKey, ts.Trigger)
	lockStart := time.Now()
	sessionLock.Lock()
	lockDur := time.Since(lockStart)
	a.logTurnLockWait(ts.SessionKey, lockDur, ts.Trigger)
	return sessionLock.Unlock
}

// IncrementProcessing bumps the atomic processing counter.
// Extracted from agent.go:318-319.
func (t *APITransport) IncrementProcessing(ts *TurnState) func() {
	atomic.AddInt32(&t.agent.processing, 1)
	return func() { atomic.AddInt32(&t.agent.processing, -1) }
}

// RegisterTurn adds a TurnDetail for shutdown diagnostics.
// Extracted from agent.go:321-327.
func (t *APITransport) RegisterTurn(ts *TurnState) func() {
	td := &TurnDetail{
		SessionKey: ts.SessionKey,
		Trigger:    ts.Trigger,
		StartTime:  time.Now(),
	}
	ts.TurnDetail = td
	ts.TurnID = t.agent.registerTurn(td)
	return func() { t.agent.unregisterTurn(ts.TurnID) }
}

// --- Phase 2: Turn preparation ---

// ComposePrompt builds the user message with content blocks.
// Extracted from agent.go:409-501 (model resolution through nudge injection
// are separate methods; this handles message preparation).
func (t *APITransport) ComposePrompt(ts *TurnState) error {
	a := t.agent

	// Resolve mana for meta prefix display.
	// (prepareUserMessage does this internally, but we need effective duplicate check first.)
	// Duplicate messages: suppress when thinking is active with effort > low.
	ts.EffectiveDuplicate = a.DuplicateMessages
	if ts.EffectiveDuplicate && ts.TurnThinking != "" && ts.TurnThinking != "off" && ts.TurnEffort != "low" {
		ts.EffectiveDuplicate = false
		a.logger().Debugf("session=%s duplicate_messages suppressed: thinking=%s effort=%s",
			ts.SessionKey, ts.TurnThinking, ts.TurnEffort)
	}

	// Consume branch orientation.
	orientation := a.Sessions.ConsumeOrientation(ts.SessionKey, a.SessionIndex)

	ts.UserMsg = a.prepareUserMessage(ts.Ctx, ts.SessionKey, ts.Texts, ts.TurnModel,
		ts.Attachments, ts.EffectiveDuplicate, orientation)
	ts.Messages = append(ts.Messages, ts.UserMsg)
	ts.NewMessages = append(ts.NewMessages, ts.UserMsg)

	return nil
}

// LoadAndRepairSession loads the session history and runs repair passes.
// Extracted from agent.go:357-407.
func (t *APITransport) LoadAndRepairSession(ts *TurnState) error {
	a := t.agent

	messages, err := a.Sessions.LoadFull(ts.SessionKey)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	if loadStats := provider.ComputeSessionStats(messages); loadStats.Messages > 0 {
		a.logger().Debugf("session_loaded session=%s messages=%d blocks=%d bytes=%d tokens≈%d",
			ts.SessionKey, loadStats.Messages, loadStats.Blocks, loadStats.ApproxBytes, loadStats.ApproxTokens())
	}

	// Repair 1: interrupted tool calls.
	if repair := repairInterruptedToolCalls(messages); len(repair) > 0 {
		messages = append(messages, repair...)
		writer := a.Sessions.For(ts.SessionKey)
		if err := writer.AppendAll(ts.SessionKey, repair); err != nil {
			a.logger().Errorf("session=%s persist tool call repair: %v", ts.SessionKey, err)
		} else {
			a.logger().Infof("session=%s repaired %d interrupted tool calls", ts.SessionKey, len(repair[0].Content))
		}
	}

	// Repair 2: duplicate tool IDs.
	messages, _ = repairDuplicateToolIDs(messages, func(format string, args ...any) {
		a.logger().Warnf("session=%s "+format, append([]any{ts.SessionKey}, args...)...)
	})

	// Repair 3: missing assistant messages.
	if repaired, n := repairMissingAssistantMessages(messages); n > 0 {
		messages = repaired
		sessionFile := ts.SessionKey
		if p, err := a.Sessions.SessionPath(ts.SessionKey); err == nil {
			sessionFile = p
		}
		a.logger().Warnf("session=%s repaired %d missing/empty assistant messages in %s", ts.SessionKey, n, sessionFile)
		writer := a.Sessions.For(ts.SessionKey)
		if err := writer.Replace(ts.SessionKey, messages); err != nil {
			a.logger().Errorf("session=%s persist assistant message repair: %v", ts.SessionKey, err)
		}
	}

	ts.Messages = messages
	return nil
}

// ResolveModelEffort resolves model, client, effort, thinking, and speed
// for this turn from session overrides and agent/model defaults.
// Extracted from agent.go:409-429.
func (t *APITransport) ResolveModelEffort(ts *TurnState) {
	a := t.agent

	ts.TurnModel = a.SessionModel(ts.SessionKey)
	ts.TurnClient = a.SessionClient(ts.SessionKey)
	ts.TurnEffort = a.SessionEffort(ts.SessionKey)
	ts.TurnThinking = a.SessionThinking(ts.SessionKey)
	ts.TurnSpeed = a.SessionSpeed(ts.SessionKey)

	// Apply per-model defaults as fallback.
	if a.ModelDefaultsFn != nil {
		md := a.ModelDefaultsFn(ts.TurnModel)
		if ts.TurnEffort == "" {
			ts.TurnEffort = md.Effort
		}
		if ts.TurnThinking == "" {
			ts.TurnThinking = md.Thinking
		}
		if ts.TurnSpeed == "" {
			ts.TurnSpeed = md.Speed
		}
	}
}

// BuildSystemAndTools builds system prompt blocks and tool definitions.
// Extracted from agent.go:466-471.
func (t *APITransport) BuildSystemAndTools(ts *TurnState) {
	a := t.agent
	ts.System = a.buildSystemBlocks(ts.SessionKey)
	ts.ToolDefs = a.Tools.ToolDefs()
	if len(a.ServerTools) > 0 {
		ts.ToolDefs = append(ts.ToolDefs, a.ServerTools...)
	}
}

// InjectNudges prepends behavioral nudge reminders as content blocks.
// Extracted from agent.go:484-501.
func (t *APITransport) InjectNudges(ts *TurnState) {
	a := t.agent
	if a.Nudger == nil {
		return
	}
	a.Nudger.StartTurn(ts.Texts[0])

	var nudgeBlocks []provider.ContentBlock
	for _, r := range a.Nudger.CheckTurnInterval() {
		nudgeBlocks = append(nudgeBlocks, provider.ContentBlock{Type: "text", Text: nudgeHeader + r})
	}
	for _, r := range a.Nudger.CheckRegex() {
		nudgeBlocks = append(nudgeBlocks, provider.ContentBlock{Type: "text", Text: nudgeHeader + r})
	}
	if len(nudgeBlocks) > 0 {
		ts.UserMsg.Content = append(nudgeBlocks, ts.UserMsg.Content...)
		// Update the already-appended message in both slices.
		ts.Messages[len(ts.Messages)-1] = ts.UserMsg
		ts.NewMessages[len(ts.NewMessages)-1] = ts.UserMsg
		a.logger().Infof("nudge: %d trigger(s) prepended to user message for session %s",
			len(nudgeBlocks), ts.SessionKey)
	}
}

// --- Phase 3: Core execution ---

// RunInference runs the API tool loop. This is the largest extraction —
// the entire for loop from agent.go:521-811 plus the safety-net defer.
// On completion, closes ts.CompletionChan and sets ts.FinalText/FinalUsage.
func (t *APITransport) RunInference(ts *TurnState) error {
	a := t.agent

	maxLoops := a.MaxToolLoops
	if maxLoops <= 0 {
		maxLoops = 25
	}
	maxOutput := a.MaxOutputTokens
	if maxOutput <= 0 {
		maxOutput = 16384
	}

	var md config.ModelDefaults
	if a.ModelDefaultsFn != nil {
		md = a.ModelDefaultsFn(ts.TurnModel)
	}

	displayNoted := false
	verified := false
	var toolCallCount int
	var lastToolError bool
	var batchedText strings.Builder

	// sendOrBatchText delivers text respecting batch mode.
	sendOrBatchText := func(r provider.MessageResponse) {
		if text := provider.TextOf(r.Content); text != "" {
			if a.BatchPartialAssistantMessages {
				if batchedText.Len() > 0 {
					batchedText.WriteString(a.BatchPartialJoiner)
				}
				batchedText.WriteString(text)
			} else {
				sendIntermediateCtx(ts.Ctx, text)
				a.logConversationSent(ts.ConvChatID, ts.Meta, ts.SessionKey, text)
			}
		}
	}

	for i := 0; i < maxLoops; i++ {
		ts.Messages = sanitizeEmptyTextBlocks(ts.Messages)

		req := &provider.MessageRequest{
			Model:         ts.TurnModel,
			MaxTokens:     maxOutput,
			System:        ts.System,
			Messages:      ts.Messages,
			Tools:         ts.ToolDefs,
			CacheStrategy: a.CacheStrategy,
			CacheTTL:      md.CacheTTL,
		}
		if ts.TurnEffort != "" && ts.TurnEffort != "off" {
			req.Output = &provider.OutputConfig{Effort: ts.TurnEffort}
		}
		if ts.TurnThinking == "adaptive" {
			req.Thinking = &provider.ThinkingConfig{Type: "adaptive"}
		}
		if ts.TurnSpeed == "fast" {
			req.Speed = "fast"
		}

		logCacheDebug(ts.SessionKey, ts.System, ts.Messages, ts.TurnModel)
		a.logger().Debugf("api_request session=%s model=%s messages=%d tools=%d system_blocks=%d",
			ts.SessionKey, ts.TurnModel, len(ts.Messages), len(ts.ToolDefs), len(ts.System))

		start := time.Now()
		a.logger().Debugf("api_call_start session=%s model=%s streaming=%v", ts.SessionKey, ts.TurnModel, a.Streaming)

		var handler *provider.StreamHandler
		if a.Streaming {
			handler = &provider.StreamHandler{
				OnTextDelta: func(delta string) {
					notifyTextDeltaCtx(ts.Ctx, delta)
					signalActivityCtx(ts.Ctx)
				},
				OnThinkingDelta: func(delta string) {
					notifyThinkingDeltaCtx(ts.Ctx, delta)
				},
			}
		}

		ctx := provider.WithRetryCallbacks(ts.Ctx, &provider.RetryCallbacks{
			OnFirstRetry: func(endpoint string) {
				notifyRetryCtx(ts.Ctx, endpoint)
			},
			OnSuccess: func() {
				notifyRetrySuccessCtx(ts.Ctx)
			},
		})

		resp, err := provider.Send(ctx, ts.TurnClient, req, handler,
			a.FallbackFunc, a.ClientProvider, func(f string, args ...any) {
				a.logger().Warnf("session=%s "+f, append([]any{ts.SessionKey}, args...)...)
			})

		duration := time.Since(start)
		keySuffix := ""
		if resp != nil {
			keySuffix = resp.KeySuffix
		}
		a.logger().Debugf("api_call_done session=%s duration=%s key=%s err=%v", ts.SessionKey, duration, keySuffix, err)
		req.Model = ts.TurnModel

		if err != nil {
			a.logErrorPayload(ts.SessionKey, ts.TurnModel, start, duration, req, err)

			errMsg := provider.Message{
				Role:    "assistant",
				Content: provider.TextContent("(API error — response unavailable)"),
			}
			ts.NewMessages = append(ts.NewMessages, errMsg)

			a.metaMu.Lock()
			endpoint := ts.SessionMeta.modelEndpoint
			if endpoint == "" {
				endpoint = a.Endpoint
			}
			a.metaMu.Unlock()
			return a.classifyAPIError(ts.Ctx, err, ts.SessionKey, endpoint, duration)
		}

		if ts.Ctx.Err() != nil {
			errMsg := provider.Message{
				Role:    "assistant",
				Content: provider.TextContent("(API error — response unavailable)"),
			}
			ts.NewMessages = append(ts.NewMessages, errMsg)
			return ts.Ctx.Err()
		}

		cost := a.logAPIResponse(ts.SessionKey, ts.TurnModel, start, duration, req, resp, len(ts.Messages))
		a.processAPIResponse(ts.SessionKey, ts.SessionMeta, resp, cost, ts.StartedAt, maxOutput)

		assistantMsg := provider.Message{
			Role:    resp.Role,
			Content: resp.Content,
		}
		ts.Messages = append(ts.Messages, assistantMsg)
		ts.NewMessages = append(ts.NewMessages, assistantMsg)

		notifyResponseBlocks(ts.Ctx, resp.Content)

		if resp.StopReason == "pause_turn" {
			continue
		}

		if resp.StopReason != "tool_use" {
			// Pre-answer verification gate.
			if !verified && a.Nudger != nil && a.NudgePreAnswerGate && i >= a.NudgePreAnswerMinTools {
				if reminder := a.Nudger.CheckPreAnswer(); reminder != "" {
					verifyMsg := provider.Message{
						Role:    "user",
						Content: provider.TextContent(nudgeHeader + reminder + "\n\nIf your answer stands as-is, respond with `" + NoResponseSentinel + "` and nothing else."),
					}
					ts.Messages = append(ts.Messages, verifyMsg)
					ts.NewMessages = append(ts.NewMessages, verifyMsg)
					verified = true
					a.logger().Infof("nudge: pre-answer gate fired at loop %d for session %s", i, ts.SessionKey)
					sendOrBatchText(*resp)
					continue
				}
			}

			// Turn complete — set results. Save and metadata update
			// happen in post-turn (SaveSession / UpdateSessionMeta).
			ts.FinalUsage = &resp.Usage
			ts.FinalCost = cost
			ts.FinalText = provider.TextOf(resp.Content)

			// Handle batched partial messages.
			if a.BatchPartialAssistantMessages && batchedText.Len() > 0 {
				if ts.FinalText != "" {
					batchedText.WriteString(a.BatchPartialJoiner)
					batchedText.WriteString(ts.FinalText)
				}
				ts.FinalText = batchedText.String()
			}

			close(ts.CompletionChan)
			return nil
		}

		// Handle text in tool_use responses.
		sendOrBatchText(*resp)

		// Last allowed iteration — don't execute tools, inject error results.
		if i+1 >= maxLoops {
			var toolResults []provider.ContentBlock
			errText := fmt.Sprintf("Tool call not executed: max tool loop depth reached (limit: %d)", maxLoops)
			for _, block := range resp.Content {
				if block.Type != "tool_use" {
					continue
				}
				toolResults = append(toolResults, provider.ToolResultBlock(block.ID, errText, true))
			}
			toolMsg := provider.Message{Role: "user", Content: toolResults}
			ts.NewMessages = append(ts.NewMessages, toolMsg)
			break
		}

		// Execute tool calls.
		toolResults, err := a.executeToolCalls(ts.Ctx, ts.TurnDetail, ts.TurnClient, ts.SessionKey, ts.TurnModel, resp.Content, ts.Messages)
		if err != nil {
			return err
		}

		// Strip unmatched tool_use blocks (steer skipped them).
		if filtered, stripped := stripUnmatchedToolUse(resp.Content, toolResults); stripped {
			ts.Messages[len(ts.Messages)-1].Content = filtered
			ts.NewMessages[len(ts.NewMessages)-1].Content = filtered
		}

		// Track tool calls and errors for nudge triggers.
		lastToolError = false
		for _, block := range resp.Content {
			if block.Type == "tool_use" {
				toolCallCount++
			}
		}
		for _, tr := range toolResults {
			if tr.Type == "tool_result" && tr.IsError {
				lastToolError = true
				break
			}
		}

		// Tool display note (once per turn).
		if !displayNoted {
			toolResults = append(toolResults, provider.ContentBlock{
				Type: "text",
				Text: toolDisplayNote(a.SessionShowToolCalls(ts.SessionKey)),
			})
			displayNoted = true
		}

		// Nudge reminders.
		if a.Nudger != nil {
			if reminders := a.Nudger.CheckAfterTools(toolCallCount, lastToolError); len(reminders) > 0 {
				for _, r := range reminders {
					toolResults = append(toolResults, provider.ContentBlock{Type: "text", Text: nudgeHeader + r})
				}
				a.logger().Debugf("nudge: injected %d reminder(s) at loop %d for session %s", len(reminders), i, ts.SessionKey)
			}
		}

		// Steer check: catch messages arriving after tool batch.
		if blocks := steerBlocks(ts.Ctx); len(blocks) > 0 {
			toolResults = append(toolResults, blocks...)
			a.logger().Infof("steer: injected %d user message(s) after tool batch for session %s", len(blocks), ts.SessionKey)
		}

		toolMsg := provider.Message{Role: "user", Content: toolResults}
		ts.Messages = append(ts.Messages, toolMsg)
		ts.NewMessages = append(ts.NewMessages, toolMsg)
	}

	// Max loops reached.
	sessionFile := ts.SessionKey
	if p, err := a.Sessions.SessionPath(ts.SessionKey); err == nil {
		sessionFile = p
	}
	a.logger().Warnf("max tool call depth reached for session %s", sessionFile)

	ts.FinalText = "Max tool call depth reached."
	close(ts.CompletionChan)
	return nil
}

// --- Phase 4: Post-turn ---

// SaveSession persists new messages accumulated during the turn.
// On success, nils NewMessages so the safety-net defer in RunInference
// won't double-save.
func (t *APITransport) SaveSession(ts *TurnState) error {
	if len(ts.NewMessages) == 0 {
		return nil
	}
	a := t.agent
	writer := a.Sessions.For(ts.SessionKey)
	if err := writer.AppendAll(ts.SessionKey, ts.NewMessages); err != nil {
		return err
	}
	ts.NewMessages = nil // saved — RunInference's safety-net defer won't double-save

	endStats := provider.ComputeSessionStats(ts.Messages)
	a.logger().Debugf("turn_end session=%s messages=%d blocks=%d bytes=%d tokens≈%d",
		ts.SessionKey, endStats.Messages, endStats.Blocks, endStats.ApproxBytes, endStats.ApproxTokens())
	return nil
}

// UpdateSessionMeta updates per-session cost/token tracking from the
// completed turn. Per-iteration cache tracking (processAPIResponse)
// still happens inside RunInference's loop; this handles the final update.
func (t *APITransport) UpdateSessionMeta(ts *TurnState) {
	if ts.SessionMeta == nil || ts.FinalUsage == nil {
		return
	}
	ts.SessionMeta.lastMessageTime = ts.StartedAt
	ts.SessionMeta.prevCost = ts.FinalCost
	ts.SessionMeta.prevInput = ts.FinalUsage.InputTokens
	ts.SessionMeta.prevOutput = ts.FinalUsage.OutputTokens
	ts.SessionMeta.prevCacheWrite = ts.FinalUsage.CacheCreationInputTokens
}

// LogUsage is a no-op for API — usage is logged per-call inside RunInference
// via logAPIResponse. Self-invoked contract method, not called by orchestrator.
func (t *APITransport) LogUsage(ts *TurnState) {}

// RunCompaction checks if compaction is needed and runs it.
// Extracted from agent.go:690.
func (t *APITransport) RunCompaction(ts *TurnState) {
	a := t.agent
	if ts.FinalUsage == nil {
		return
	}
	a.maybeCompact(ts.Ctx, ts.SessionKey, ts.Messages, ts.System, ts.FinalUsage, ts.SessionMeta)
}

// ---------------------------------------------------------------------------
// API transport helpers — only called from APITransport methods above.
// ---------------------------------------------------------------------------

// logTurnLockWait logs a warning when the turn lock was held longer than the
// configured threshold, including details about the current holder if found.
func (a *Agent) logTurnLockWait(sessionKey string, lockDur time.Duration, waiterTrigger string) {
	warnThreshold := a.TurnLockWarnThreshold
	if warnThreshold <= 0 {
		warnThreshold = 3 * time.Minute
	}
	if lockDur > warnThreshold && waiterTrigger != "proactive_warning" {
		holder := ""
		for _, td := range a.ProcessingDetails() {
			if td.SessionKey == sessionKey {
				holder = fmt.Sprintf(" holder_trigger=%s holder_tool=%s holder_elapsed=%s",
					td.Trigger, td.ToolName, time.Since(td.StartTime).Truncate(time.Millisecond))
				break
			}
		}
		a.logger().Warnf("turn_lock_held session=%s waited=%s waiter_trigger=%s%s", sessionKey, lockDur, waiterTrigger, holder)
	} else {
		a.logger().Debugf("turn_lock_acquired session=%s waited=%s", sessionKey, lockDur)
	}
}

// registerTurn adds a TurnDetail and returns its ID.
func (a *Agent) registerTurn(d *TurnDetail) uint64 {
	id := atomic.AddUint64(&a.turnIDCounter, 1)
	a.turnDetailsMu.Lock()
	if a.turnDetails == nil {
		a.turnDetails = make(map[uint64]*TurnDetail)
	}
	a.turnDetails[id] = d
	a.turnDetailsMu.Unlock()
	return id
}

// unregisterTurn removes a TurnDetail by ID.
func (a *Agent) unregisterTurn(id uint64) {
	a.turnDetailsMu.Lock()
	delete(a.turnDetails, id)
	a.turnDetailsMu.Unlock()
}

// logConversationSent logs an outbound conversation entry.
func (a *Agent) logConversationSent(chatID int64, meta *TurnMetadata, sessionKey, text string) {
	if text == "" {
		return
	}
	log.Conversation(log.ConversationEntry{
		Direction: "sent",
		UserID:    meta.UserID,
		Username:  meta.Username,
		ChatID:    chatID,
		Text:      text,
		Session:   sessionKey,
	})
}

// toolDisplayNote returns a short system note describing whether the user can see tool results.
func toolDisplayNote(mode string) string {
	switch mode {
	case "full":
		return "[display] tool_results=visible — the user can see your tool calls, inputs, and outputs, so you may refer to those rather than restate them in full, but you should still narrate the outline of what you are doing or what you have learned."
	case "preview":
		return "[display] tool_results=preview — the user sees tool names and inputs briefly, but not inputs or outputs."
	default:
		return "[display] tool_results=hidden — the user cannot see tool calls or results. Narrate important actions and findings in your replies."
	}
}

