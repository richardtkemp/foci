package agent

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"foci/internal/agent/turnevent"
	"foci/internal/compaction"
	"foci/internal/delegator"
	"foci/internal/log"
	"foci/internal/provider"
)

// ---------------------------------------------------------------------------
// DelegatedTransport — TurnContract implementations for the delegated transport
// path. Methods that are genuinely no-ops return zero values with a comment
// explaining why. The transport explicitly opts out rather than silently skipping.
// ---------------------------------------------------------------------------

// --- Phase 1: No-ops (CC handles these internally) ---

func (t *DelegatedTransport) RateLimitGate(ts *TurnState) error        { return nil }       // CC has its own rate limiting
func (t *DelegatedTransport) AcquireTurnLock(ts *TurnState) func()     { return func() {} } // CC serializes internally
func (t *DelegatedTransport) IncrementProcessing(ts *TurnState) func() { // tracks IsProcessing() for shutdown/reset gating
	atomic.AddInt32(&t.agent.processing, 1)
	return func() { atomic.AddInt32(&t.agent.processing, -1) }
}
func (t *DelegatedTransport) RegisterTurn(ts *TurnState) func()        { return func() {} } // not tracked externally

// --- Phase 2: Turn preparation ---

// ResolveModelEffort uses the actual model learned from the JSONL watcher
// (stored in sessionMeta by UpdateSessionMeta), falling back to the
// agent-level model name before the first turn completes.
func (t *DelegatedTransport) ResolveModelEffort(ts *TurnState) {
	if m := t.agent.SessionModel(ts.SessionKey); m != "" {
		ts.TurnModel = m
		return
	}
	ts.TurnModel = t.agent.Model
}

// ComposePrompt builds a flat text prompt via composeTurnText + JoinPrompt.
// Extracted from the original backend_turn.go.
func (t *DelegatedTransport) ComposePrompt(ts *TurnState) error {
	a := t.agent

	parts := a.composeTurnText(ts.Ctx, ts.SessionKey, ts.TurnModel, "", false, ts.Texts, ts.Attachments)
	ts.Prompt = parts.JoinPrompt()

	// Consume branch orientation. ConsumeOrientation is atomic — returns the
	// orientation once and marks it consumed in the DB, same as API transport.
	if a.Sessions != nil {
		if orient := a.Sessions.ConsumeOrientation(ts.SessionKey, a.SessionIndex); orient != "" {
			ts.Prompt = orient + "\n\n" + ts.Prompt
			log.Infof("delegated", "session=%s injected branch orientation (%d chars)", ts.SessionKey, len(orient))
		}
	}

	// Update lastMessageTime AFTER composition so the gap is calculated
	// against the previous message, not the current one.
	ts.SessionMeta.lastMessageTime = ts.UserMessageTime()

	return nil
}

// LoadAndRepairSession is a no-op — CC owns its session file.
func (t *DelegatedTransport) LoadAndRepairSession(ts *TurnState) error { return nil }

// BuildSystemAndTools is a no-op — system prompt and tools are set at Start time.
func (t *DelegatedTransport) BuildSystemAndTools(ts *TurnState) {}

// InjectNudges prepends behavioral nudge reminders to the prompt string.
func (t *DelegatedTransport) InjectNudges(ts *TurnState) {
	a := t.agent
	if a.Nudger == nil || len(ts.Texts) == 0 {
		return
	}
	a.Nudger.StartTurn(ts.Texts[0])

	var nudges []string
	for _, r := range a.Nudger.CheckTurnInterval() {
		nudges = append(nudges, nudgeHeader+r)
	}
	for _, r := range a.Nudger.CheckRegex() {
		nudges = append(nudges, nudgeHeader+r)
	}
	if len(nudges) > 0 {
		ts.Prompt = strings.Join(nudges, "\n") + "\n" + ts.Prompt
	}
}

// --- Phase 3: Core execution ---

// RunInference sends the composed prompt to the backend with a per-turn
// completion handler that closes CompletionChan and captures results.
//
// If a turn is already in-flight (steered follow-up message), the prompt
// is pasted into the pane via SendCommand without registering a new callback.
// CC treats both messages as part of one turn. The original turn's handler
// captures the combined result. This turn's CompletionChan is closed
// immediately and runPostTurn skips it (no post-turn work needed).
func (t *DelegatedTransport) RunInference(ts *TurnState) error {
	a := t.agent

	be, err := a.DelegatedManager.Get(ts.Ctx, ts.SessionKey)
	if err != nil {
		return err
	}
	ts.Backend = be

	// Check for a pending AskUserQuestion — intercept typed text as an
	// answer before WaitForPermission blocks. This lets users respond to
	// questions by typing instead of clicking buttons.
	if qr, ok := be.(delegator.QuestionResponder); ok {
		if reqID := qr.HasPendingQuestion(); reqID != "" && len(ts.Texts) > 0 {
			text := strings.TrimSpace(ts.Texts[0])
			if text != "" {
				log.Debugf("delegated", "session=%s intercepting text as question answer: %q", ts.SessionKey, text)
				_ = qr.RespondToQuestion(reqID, text)
				close(ts.CompletionChan)
				return nil
			}
		}
	}

	// Same intercept for pending elicitation form fields — typed text
	// becomes the answer for the current free-text field.
	if er, ok := be.(delegator.ElicitationResponder); ok {
		if reqID := er.HasPendingElicitation(); reqID != "" && len(ts.Texts) > 0 {
			text := strings.TrimSpace(ts.Texts[0])
			if text != "" {
				log.Debugf("delegated", "session=%s intercepting text as elicitation answer: %q", ts.SessionKey, text)
				_ = er.RespondToElicitation(reqID, text)
				close(ts.CompletionChan)
				return nil
			}
		}
	}

	// Wait for any outstanding permission prompt to resolve before sending
	// new input. The backend cannot process messages while blocked on a
	// permission decision. Messages queue naturally in the platform's
	// MessageQueue channel while the worker goroutine blocks here.
	if err := a.DelegatedManager.WaitForPermission(ts.Ctx, ts.SessionKey); err != nil {
		return err
	}

	// Follow-up: a turn is already in-flight. Send the text to CC without
	// creating a new turn pipeline. CC queues it after the current turn
	// (priority "next"). Close CompletionChan immediately so runPostTurn
	// exits without doing any post-turn work (the original turn's handler
	// will handle usage, compaction, etc.).
	//
	// Note: urgent steers (steer_mode=true) don't reach this path — they're
	// buffered in steerParts and injected mid-turn by the ccstream backend's
	// checkAndSendSteers with priority "now". This path handles the
	// steer_mode=false case where messages flow through the channel normally.
	if be.IsTurnInFlight() {
		log.Infof("delegated", "session=%s follow-up message queued behind in-flight turn", ts.SessionKey)
		if err := be.SendCommand(ts.Ctx, ts.Prompt, "next"); err != nil {
			close(ts.CompletionChan)
			return err
		}
		close(ts.CompletionChan)
		return nil
	}

	// Cache session file path BEFORE SendToPane — the OnTurnComplete callback
	// may fire inside ensureWatcher (which holds b.mu), and SessionFilePath()
	// also needs b.mu, causing a deadlock.
	ts.sessionFilePath = be.SessionFilePath()

	// Per-turn handler: fires once when the watcher sees end_turn.
	// Captures FinalText/FinalUsage/FinalModel, logs usage, then closes CompletionChan.
	bt := t

	// Cumulative tool-call state for every_n_tools / after_error nudges.
	// These fire via PostToolNudgeFunc, once per tool hook_response — the
	// scheduler's internal cooldown prevents a rule from re-firing on
	// back-to-back tools. Lives in a closure so that each turn starts
	// at zero without polluting DelegatedTransport's long-lived state.
	var toolCount int

	// Pre-answer gate state: when PreAnswerNudgeFunc returns a follow-up,
	// ccstream re-dispatches this handler for a second round. preAnswerFired
	// flips to true on first return so subsequent calls from the second
	// round's OnResult yield "" and break the loop. preAnswerAccumulated
	// folds round-1 usage into the final accounting — ccstream's beginTurn
	// resets lastUsage between rounds, so we stash it here.
	var (
		preAnswerFired       bool
		preAnswerAccumulated *provider.Usage
		preAnswerFirstText   string
	)

	// Translate the delegator watcher's callbacks into turnevent.Sink events.
	// OnTextDelta feeds the stream writer via TextDelta events (edit-in-place
	// streaming). OnText delivers complete text blocks as intermediate TextBlocks
	// so sinks can drive renderer.OnReply. The StreamingSink owns the delivered
	// flag that used to live on the renderer — so intermediate delivery during
	// the turn correctly suppresses re-delivery of the final text.
	turnCtx := ts.Ctx
	handler := &delegator.EventHandler{
		OnTextDelta: func(delta string) {
			turnevent.Emit(turnCtx, turnevent.TextDelta{Delta: delta})
		},
		OnThinkingDelta: func(delta string) {
			turnevent.Emit(turnCtx, turnevent.ThinkingDelta{Delta: delta})
		},
		OnText: func(text string) {
			turnevent.Emit(turnCtx, turnevent.TextBlock{Text: text, Phase: turnevent.PhaseIntermediate})
			// Log each intermediate text individually to the conversation DB.
			// The API transport does this in sendOrBatchText; the delegated
			// path was missing it, causing conversation.db to only record a
			// single concatenated row at turn end.
			a.logConversationSent(ts.ConvChatID, ts.Meta, ts.SessionKey, text)
		},
		OnToolStart: func(id, name, input string) {
			turnevent.Emit(turnCtx, turnevent.ToolCall{ID: id, Name: name, Args: []byte(input)})
		},
		OnToolEnd: func(id, name, output string, isError bool) {
			turnevent.Emit(turnCtx, turnevent.ToolResult{ID: id, Name: name, Output: output, IsError: isError})
		},
		PostToolNudgeFunc: func(toolName string, isError bool) []string {
			if a.Nudger == nil {
				return nil
			}
			toolCount++
			reminders := a.Nudger.CheckAfterTools(toolCount, isError)
			if len(reminders) == 0 {
				return nil
			}
			out := make([]string, 0, len(reminders))
			for _, r := range reminders {
				out = append(out, nudgeHeader+r)
			}
			a.logger().Debugf("nudge: injected %d reminder(s) after tool %q (count=%d, err=%v) for session %s",
				len(out), toolName, toolCount, isError, ts.SessionKey)
			return out
		},
		PreAnswerNudgeFunc: func(result *delegator.TurnResult) string {
			if preAnswerFired || a.Nudger == nil || !a.NudgePreAnswerGate {
				return ""
			}
			if toolCount < a.NudgePreAnswerMinTools {
				return ""
			}
			reminder := a.Nudger.CheckPreAnswer()
			if reminder == "" {
				return ""
			}
			preAnswerFired = true
			// Stash round-1 state so the final OnTurnComplete can fold it
			// in. The original answer has already streamed to the user
			// (OnText delivered PhaseIntermediate), so the sink will treat
			// the round-2 result as the authoritative final text.
			if result != nil {
				preAnswerFirstText = result.Text
				if result.Usage != nil {
					preAnswerAccumulated = &provider.Usage{
						InputTokens:              result.Usage.InputTokens,
						OutputTokens:             result.Usage.OutputTokens,
						CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
						CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
					}
				}
			}
			a.logger().Infof("nudge: pre-answer gate fired for session %s (tool_count=%d)",
				ts.SessionKey, toolCount)
			return nudgeHeader + reminder + "\n\nIf your answer stands as-is, respond with `" + NoResponseSentinel + "` and nothing else."
		},
	}
	handler.OnTurnComplete = func(result *delegator.TurnResult) {
		if result != nil {
			ts.FinalText = result.Text
			ts.FinalModel = result.Model
			if result.Usage != nil {
				ts.FinalUsage = &provider.Usage{
					InputTokens:              result.Usage.InputTokens,
					OutputTokens:             result.Usage.OutputTokens,
					CacheCreationInputTokens: result.Usage.CacheCreationInputTokens,
					CacheReadInputTokens:     result.Usage.CacheReadInputTokens,
				}
			}
		}
		// Fold round-1 usage into the final totals when the pre-answer
		// gate ran a second round. Token cost is the sum of both rounds
		// so the API log reflects the true spend of the user's turn.
		if preAnswerAccumulated != nil && ts.FinalUsage != nil {
			ts.FinalUsage.InputTokens += preAnswerAccumulated.InputTokens
			ts.FinalUsage.OutputTokens += preAnswerAccumulated.OutputTokens
			ts.FinalUsage.CacheCreationInputTokens += preAnswerAccumulated.CacheCreationInputTokens
			ts.FinalUsage.CacheReadInputTokens += preAnswerAccumulated.CacheReadInputTokens
		}
		// If the model chose to echo the sentinel the API path uses to
		// mean "my original answer stands", replace the final text with
		// the round-1 answer so the platform delivery reflects the
		// user-visible intent. Otherwise the round-2 revised answer wins.
		if preAnswerFirstText != "" && strings.TrimSpace(ts.FinalText) == NoResponseSentinel {
			ts.FinalText = preAnswerFirstText
		}
		bt.LogUsage(ts)
		close(ts.CompletionChan)
	}
	// Wire steer drain from the context-attached Steerer into the backend
	// so ccstream can inject steered messages at tool execution boundaries.
	if st := turnevent.SteererFromContext(ts.Ctx); st != nil {
		handler.SteerCheckFunc = st.PendingSteers
	}

	// Use structured content blocks when attachments are present and the
	// backend supports it. This sends images/documents as proper content
	// blocks instead of flattening them into the text prompt.
	if len(ts.Attachments) > 0 {
		if as, ok := be.(delegator.AttachmentSender); ok {
			atts := make([]delegator.Attachment, len(ts.Attachments))
			for i, a := range ts.Attachments {
				atts[i] = delegator.Attachment{
					MimeType: a.MimeType,
					Data:     a.Data,
				}
			}
			_, err = as.SendToPaneWithAttachments(ts.Ctx, ts.Prompt, atts, handler)
			return err
		}
	}

	_, err = be.SendToPane(ts.Ctx, ts.Prompt, handler)
	return err
}

// --- Phase 4: Post-turn ---

// LogConversationSent is a no-op — the delegated path logs each intermediate
// text individually in the OnText callback. The shared implementation would
// log a single concatenated blob of all text from the turn, which is both
// redundant and incorrect (it concatenates separate messages into one row).
func (t *DelegatedTransport) LogConversationSent(ts *TurnState) {}

// SaveSession is a no-op — CC owns its session file.
func (t *DelegatedTransport) SaveSession(ts *TurnState) error { return nil }

// UpdateSessionMeta updates per-session token tracking from the
// JSONL-extracted usage. Cost calculation is not available for delegated
// turns (no logAPIResponse), so prevCost stays zero.
func (t *DelegatedTransport) UpdateSessionMeta(ts *TurnState) {
	if ts.SessionMeta == nil || ts.FinalUsage == nil {
		return
	}
	ts.SessionMeta.lastMessageTime = ts.UserMessageTime()
	ts.SessionMeta.prevInput = ts.FinalUsage.InputTokens
	ts.SessionMeta.prevOutput = ts.FinalUsage.OutputTokens
	ts.SessionMeta.prevCacheRead = ts.FinalUsage.CacheReadInputTokens
	ts.SessionMeta.prevCacheWrite = ts.FinalUsage.CacheCreationInputTokens

	// Record the actual model reported by the backend so that
	// SessionContextLimit uses the real context window. The modelUserSet flag
	// protects against in-flight turns clobbering a freshly-set /model alias,
	// but is cleared immediately after so that the next turn resolves the alias
	// to its full name (e.g. "sonnet" → "claude-sonnet-4-5-...").
	if ts.FinalModel != "" && !ts.SessionMeta.modelUserSet {
		ts.SessionMeta.model = ts.FinalModel
	}
	ts.SessionMeta.modelUserSet = false
}

// LogUsage records delegated turn usage to the API database.
// Self-invoked from the post-turn path after FinalUsage is populated.
func (t *DelegatedTransport) LogUsage(ts *TurnState) {
	if ts.FinalUsage == nil {
		return
	}
	a := t.agent
	model := ts.FinalModel
	if model == "" {
		model = ts.TurnModel
	}
	cost := log.CalculateCost(model,
		ts.FinalUsage.InputTokens, ts.FinalUsage.OutputTokens,
		ts.FinalUsage.CacheReadInputTokens, ts.FinalUsage.CacheCreationInputTokens)
	ts.FinalCost = cost

	// Use cached path — SessionFilePath() takes b.mu which may be held
	// by ensureWatcher when this is called from the OnTurnComplete callback.
	sessionFile := ts.sessionFilePath
	log.API(log.APIEntry{
		Timestamp:   ts.StartedAt,
		Provider:    "anthropic",
		Session:     ts.SessionKey,
		Model:       model,
		Input:       ts.FinalUsage.InputTokens,
		Output:      ts.FinalUsage.OutputTokens,
		CacheRead:   ts.FinalUsage.CacheReadInputTokens,
		CacheWrite:  ts.FinalUsage.CacheCreationInputTokens,
		CostUSD:     cost,
		DurationMS:  time.Since(ts.StartedAt).Milliseconds(),
		StopReason:  "end_turn",
		CallType:    "delegated_turn",
		SessionFile: sessionFile,
	})

	a.logger().Infof("session=%s model=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f (delegated)",
		ts.SessionKey, model, ts.FinalUsage.InputTokens, ts.FinalUsage.OutputTokens,
		ts.FinalUsage.CacheReadInputTokens, ts.FinalUsage.CacheCreationInputTokens, cost)
}

// RunCompaction checks whether context compaction is needed and dispatches
// to the shared runDelegatedCompact primitive when the threshold is hit.
// Two triggers: (1) standard threshold (e.g. 80% of context window),
// (2) mana-refresh (lower threshold when Anthropic rate limit resets soon).
// The watcher's TurnUsage provides the token counts; the context window
// comes from model metadata.
func (t *DelegatedTransport) RunCompaction(ts *TurnState) {
	a := t.agent
	if a.Compactor == nil || ts.FinalUsage == nil {
		return
	}

	totalTokens := ts.FinalUsage.InputTokens + ts.FinalUsage.CacheReadInputTokens + ts.FinalUsage.CacheCreationInputTokens
	ctxLimit := a.SessionContextLimit(ts.SessionKey)
	if ctxLimit <= 0 {
		return
	}

	// Check mana-refresh trigger: compact at a lower threshold when mana
	// reset is imminent so the new window starts with a smaller context.
	isManaRefresh := false
	if a.AutocompactBeforeManaRefresh {
		usageClient := a.SessionUsageClient(ts.SessionKey)
		if usageClient != nil {
			manaRefreshThreshold := parseDurationFallback(a.AutocompactBeforeManaRefreshThreshold, 5*time.Minute)
			if w, err := usageClient.GetUsage(ts.Ctx); err == nil && w != nil && !w.ResetsAt.IsZero() {
				if compaction.ManaResetImminent(w.ResetsAt, manaRefreshThreshold) {
					secondaryThreshold := int(float64(ctxLimit) * a.Compactor.Threshold() * a.AutocompactBeforeManaRefreshFactor)
					if totalTokens > secondaryThreshold {
						isManaRefresh = true
						untilReset := time.Until(w.ResetsAt).Round(time.Minute)
						a.logger().Infof("session=%s mana-refresh compaction (reset in %s, %d/%d tokens, delegated)",
							ts.SessionKey, untilReset, totalTokens, ctxLimit)
					}
				}
			}
		}
	}

	// Standard threshold check (if mana-refresh didn't trigger).
	// Delegated transport has no messages slice — use usage-only check.
	if !isManaRefresh {
		threshold := int(float64(ctxLimit) * a.Compactor.Threshold())
		if totalTokens <= threshold {
			return
		}
		a.logger().Infof("session=%s hit threshold: %d/%d tokens (%d%%, delegated)",
			ts.SessionKey, totalTokens, ctxLimit, int(float64(totalTokens)/float64(ctxLimit)*100))
	}

	if a.SessionNoCompact(ts.SessionKey) {
		percent := int(float64(totalTokens) / float64(ctxLimit) * 100)
		a.logger().Infof("session=%s context at %d%% capacity for no_compact session (delegated)", ts.SessionKey, percent)
		return
	}

	// Memory formation runs only on auto-compaction — the post-turn hook is
	// where the agent records insights before the window shrinks.
	for _, fn := range a.CompactionMemoryFunc {
		fn(ts.SessionKey)
	}

	// Background — not ts.Ctx which may be cancelled (post-turn runs after
	// processAgentMessage returns and the turn context is done).
	if err := a.runDelegatedCompact(context.Background(), ts.Backend, ts.SessionKey); err != nil {
		a.logger().Warnf("session=%s delegated compaction failed: %v", ts.SessionKey, err)
		return
	}

	// Turn-specific post-compact state: per-turn session meta caches are
	// stale now that CC has rewritten the transcript.
	if ts.SessionMeta != nil {
		ts.SessionMeta.systemBlocks = nil
		ts.SessionMeta.prevCacheRead = 0
	}
}
