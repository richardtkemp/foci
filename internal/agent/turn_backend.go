package agent

import (
	"strings"
	"time"

	"foci/internal/backend"
	"foci/internal/log"
	"foci/internal/provider"
)

// ---------------------------------------------------------------------------
// BackendTransport — TurnContract implementations for the coding agent path.
// Methods that are genuinely no-ops return zero values with a comment
// explaining why. The backend explicitly opts out rather than silently skipping.
// These exist but are NOT called until Stage 6 (the switchover).
// ---------------------------------------------------------------------------

// --- Phase 1: No-ops (CC handles these internally) ---

func (t *BackendTransport) RateLimitGate(ts *TurnState) error        { return nil }       // CC has its own rate limiting
func (t *BackendTransport) AcquireTurnLock(ts *TurnState) func()     { return func() {} } // CC serializes internally
func (t *BackendTransport) IncrementProcessing(ts *TurnState) func() { return func() {} } // fire-and-forget from foci's view
func (t *BackendTransport) RegisterTurn(ts *TurnState) func()        { return func() {} } // not tracked externally

// --- Phase 2: Turn preparation ---

// ResolveModelEffort reads the agent-level model. The backend doesn't do
// per-turn model switching — the model is set at Start time.
func (t *BackendTransport) ResolveModelEffort(ts *TurnState) {
	ts.TurnModel = t.agent.Model
}

// ComposePrompt builds a flat text prompt via composeTurnText + JoinPrompt.
// Extracted from backend_turn.go:44-49.
func (t *BackendTransport) ComposePrompt(ts *TurnState) error {
	a := t.agent

	parts := a.composeTurnText(ts.Ctx, ts.SessionKey, ts.TurnModel, "", false, ts.Texts, ts.Attachments)
	ts.Prompt = parts.JoinPrompt()

	// Update lastMessageTime AFTER composition so the gap is calculated
	// against the previous message, not the current one.
	ts.SessionMeta.lastMessageTime = ts.StartedAt

	return nil
}

// LoadAndRepairSession is a no-op — CC owns its session file.
func (t *BackendTransport) LoadAndRepairSession(ts *TurnState) error { return nil }

// BuildSystemAndTools is a no-op — system prompt and tools are set at Start time.
func (t *BackendTransport) BuildSystemAndTools(ts *TurnState) {}

// InjectNudges prepends behavioral nudge reminders to the prompt string.
func (t *BackendTransport) InjectNudges(ts *TurnState) {
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

// ExecuteTurn sends the composed prompt to the backend with a per-turn
// completion handler that closes CompletionChan and captures results.
func (t *BackendTransport) ExecuteTurn(ts *TurnState) error {
	a := t.agent

	be, err := a.BackendManager.Get(ts.Ctx, ts.SessionKey)
	if err != nil {
		return err
	}
	ts.Backend = be

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

	_, err = be.SendTurn(ts.Ctx, ts.Prompt, handler)
	return err
}

// --- Phase 4: Post-turn ---

// SaveSession is a no-op — CC owns its session file.
func (t *BackendTransport) SaveSession(ts *TurnState) error { return nil }

// UpdateSessionMeta updates per-session token tracking from the
// JSONL-extracted usage. Cost calculation is not available for backend
// turns (no logAPIResponse), so prevCost stays zero.
func (t *BackendTransport) UpdateSessionMeta(ts *TurnState) {
	if ts.SessionMeta == nil || ts.FinalUsage == nil {
		return
	}
	ts.SessionMeta.lastMessageTime = ts.StartedAt
	ts.SessionMeta.prevInput = ts.FinalUsage.InputTokens
	ts.SessionMeta.prevOutput = ts.FinalUsage.OutputTokens
	ts.SessionMeta.prevCacheWrite = ts.FinalUsage.CacheCreationInputTokens
}

// LogUsage records backend turn usage to the API database.
// Self-invoked from the post-turn path after FinalUsage is populated.
func (t *BackendTransport) LogUsage(ts *TurnState) {
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
		Timestamp:   ts.StartedAt.UTC(),
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
		CallType:    "backend_turn",
		SessionFile: sessionFile,
	})

	a.logger().Infof("session=%s model=%s input=%d output=%d cache_read=%d cache_write=%d cost=$%.4f (backend)",
		ts.SessionKey, model, ts.FinalUsage.InputTokens, ts.FinalUsage.OutputTokens,
		ts.FinalUsage.CacheReadInputTokens, ts.FinalUsage.CacheCreationInputTokens, cost)
}

// RunCompaction is a stub — will send /compact command to CC when
// context window usage exceeds threshold. Needs watcher usage data (Stage 5).
func (t *BackendTransport) RunCompaction(ts *TurnState) {
	// TODO(stage5): check usage against threshold, send be.SendCommand("/compact")
}
