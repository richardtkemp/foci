package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/compaction"
	"foci/internal/log"
	"foci/internal/provider"
	"foci/shared/prompts"
)

// ---------------------------------------------------------------------------
// DelegatedTransport — TurnContract implementations for the delegated transport
// path. Methods that are genuinely no-ops return zero values with a comment
// explaining why. The transport explicitly opts out rather than silently skipping.
// ---------------------------------------------------------------------------

// --- Phase 1: No-ops (CC handles these internally) ---

func (t *DelegatedTransport) RateLimitGate(ts *TurnState) error        { return nil }       // CC has its own rate limiting
func (t *DelegatedTransport) AcquireTurnLock(ts *TurnState) func()     { return func() {} } // CC serializes internally
func (t *DelegatedTransport) IncrementProcessing(ts *TurnState) func() { return func() {} } // fire-and-forget from foci's view
func (t *DelegatedTransport) RegisterTurn(ts *TurnState) func()        { return func() {} } // not tracked externally

// --- Phase 2: Turn preparation ---

// ResolveModelEffort reads the agent-level model. The delegated transport
// doesn't do per-turn model switching — the model is set at Start time.
func (t *DelegatedTransport) ResolveModelEffort(ts *TurnState) {
	ts.TurnModel = t.agent.Model
}

// ComposePrompt builds a flat text prompt via composeTurnText + JoinPrompt.
// Extracted from the original backend_turn.go.
func (t *DelegatedTransport) ComposePrompt(ts *TurnState) error {
	a := t.agent

	parts := a.composeTurnText(ts.Ctx, ts.SessionKey, ts.TurnModel, "", false, ts.Texts, ts.Attachments)
	ts.Prompt = parts.JoinPrompt()

	// Update lastMessageTime AFTER composition so the gap is calculated
	// against the previous message, not the current one.
	ts.SessionMeta.lastMessageTime = ts.StartedAt

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

	// Wait for any outstanding permission prompt to resolve before sending
	// text to the CC pane. Sending during a permission prompt would corrupt
	// the TUI selection state. Messages queue naturally in the platform's
	// MessageQueue channel while the worker goroutine blocks here.
	if err := a.DelegatedManager.WaitForPermission(ts.Ctx, ts.SessionKey); err != nil {
		return err
	}

	// Steered follow-up: a turn is already in-flight. Paste the text into
	// the pane without creating a new turn pipeline. CC sees it as additional
	// input to the same turn. Close CompletionChan immediately so
	// runPostTurn exits without doing any post-turn work (the original
	// turn's handler will handle usage, compaction, etc.).
	if be.IsTurnInFlight() {
		log.Infof("delegated", "session=%s steered message merged into in-flight turn", ts.SessionKey)
		if err := be.SendCommand(ts.Ctx, ts.Prompt); err != nil {
			close(ts.CompletionChan)
			return err
		}
		close(ts.CompletionChan)
		return nil
	}

	// Per-turn handler: fires once when the watcher sees end_turn.
	// Captures FinalText/FinalUsage/FinalModel, logs usage, then closes CompletionChan.
	bt := t
	handler := &backend.EventHandler{
		OnTurnComplete: func(result *backend.TurnResult) {
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
			bt.LogUsage(ts)
			close(ts.CompletionChan)
		},
	}

	_, err = be.SendToPane(ts.Ctx, ts.Prompt, handler)
	return err
}

// --- Phase 4: Post-turn ---

// SaveSession is a no-op — CC owns its session file.
func (t *DelegatedTransport) SaveSession(ts *TurnState) error { return nil }

// UpdateSessionMeta updates per-session token tracking from the
// JSONL-extracted usage. Cost calculation is not available for delegated
// turns (no logAPIResponse), so prevCost stays zero.
func (t *DelegatedTransport) UpdateSessionMeta(ts *TurnState) {
	if ts.SessionMeta == nil || ts.FinalUsage == nil {
		return
	}
	ts.SessionMeta.lastMessageTime = ts.StartedAt
	ts.SessionMeta.prevInput = ts.FinalUsage.InputTokens
	ts.SessionMeta.prevOutput = ts.FinalUsage.OutputTokens
	ts.SessionMeta.prevCacheRead = ts.FinalUsage.CacheReadInputTokens
	ts.SessionMeta.prevCacheWrite = ts.FinalUsage.CacheCreationInputTokens

	// Record the actual model reported by the JSONL watcher so that
	// SessionContextLimit uses the real context window. Without this,
	// ag.Model is "claude-code" which defaults to 1M — correct as a
	// starting assumption, but the watcher knows the truth each turn.
	if ts.FinalModel != "" {
		ts.SessionMeta.model = ts.FinalModel
	}
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

	sessionFile := ""
	if ts.Backend != nil {
		sessionFile = ts.Backend.SessionFilePath()
	}
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

// RunCompaction checks whether context compaction is needed and sends
// "/compact $instructions" to CC with foci's own compaction prompt.
// Two triggers: (1) standard threshold (e.g. 80% of context window),
// (2) mana-refresh (lower threshold when Anthropic rate limit resets soon).
// The watcher's TurnUsage provides the token counts; the context window
// comes from model metadata. CC handles the actual compaction — foci
// controls when it fires and what instructions are used.
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
			if usageResp, err := usageClient.GetUsage(ts.Ctx); err == nil && usageResp.FiveHour != nil && usageResp.FiveHour.ResetsAt != nil {
				if manaResetsAt, parseErr := time.Parse(time.RFC3339Nano, *usageResp.FiveHour.ResetsAt); parseErr == nil {
					if compaction.ManaResetImminent(manaResetsAt, manaRefreshThreshold) {
						secondaryThreshold := int(float64(ctxLimit) * a.Compactor.Threshold() * a.AutocompactBeforeManaRefreshFactor)
						if totalTokens > secondaryThreshold {
							isManaRefresh = true
							untilReset := time.Until(manaResetsAt).Round(time.Minute)
							a.logger().Infof("session=%s mana-refresh compaction (reset in %s, %d/%d tokens, delegated)",
								ts.SessionKey, untilReset, totalTokens, ctxLimit)
						}
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

	// Resolve compaction instructions and send to CC.
	summaryPrompt := prompts.ResolvePrompt(a.CompactionSummaryPromptPath, "compaction-summary.md", prompts.CompactionSummary(), a.PromptSearchDirs...)
	if summaryPrompt == "" {
		a.logger().Warnf("session=%s compaction prompt is empty, skipping delegated compaction", ts.SessionKey)
		return
	}

	for _, fn := range a.CompactionMemoryFunc {
		fn(ts.SessionKey)
	}
	for _, fn := range a.CompactionStartFunc {
		fn(ts.SessionKey, "⏳ Compacting context...")
	}

	// Send "/compact $instructions" to CC — CC handles the actual compaction
	// using our prompt. This is different from the API transport which does
	// its own API call; here CC owns the session and compaction mechanics.
	cmd := fmt.Sprintf("/compact %s", summaryPrompt)
	// Use Background — not ts.Ctx which may be cancelled (post-turn runs
	// after processAgentMessage returns and the turn context is done).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := ts.Backend.SendCommand(ctx, cmd); err != nil {
		a.logger().Errorf("session=%s delegated compaction failed: %v", ts.SessionKey, err)
		return
	}

	// Wait for CC to finish the compaction turn.
	if err := ts.Backend.WaitForTurn(ctx); err != nil {
		a.logger().Warnf("session=%s timeout timeout waiting for delegated compaction: %v", ts.SessionKey, err)
	} else {
		for _, fn := range a.CompactionNotifyFunc {
			fn(ts.SessionKey, "✅ Context compacted (delegated).")
		}
	}

	// Don't reload Bootstrap here — CC owns the system prompt. Reloading
	// would make /context show disk state instead of what CC is actually using.
	// Bootstrap is only loaded on new session / /reset for delegated agents.
	if a.NudgeReloadFunc != nil {
		a.NudgeReloadFunc()
	}
	if ts.SessionMeta != nil {
		ts.SessionMeta.systemBlocks = nil
		ts.SessionMeta.prevCacheRead = 0
	}
}
