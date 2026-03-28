package agent

import (
	"context"
	"time"

	"foci/internal/backend"
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
	ts.SessionMeta.lastMessageTime = time.Now()

	return nil
}

// LoadAndRepairSession is a no-op — CC owns its session file.
func (t *BackendTransport) LoadAndRepairSession(ts *TurnState) error { return nil }

// BuildSystemAndTools is a no-op — system prompt and tools are set at Start time.
func (t *BackendTransport) BuildSystemAndTools(ts *TurnState) {}

// InjectNudges is a no-op — composeTurnText already includes nudges
// in the prompt string (via turnTextParts.Nudges → JoinPrompt).
func (t *BackendTransport) InjectNudges(ts *TurnState) {}

// --- Phase 3: Core execution ---

// ExecuteTurn sends the composed prompt to the backend and starts an async
// goroutine that waits for turn completion to close CompletionChan.
// Extracted from backend_turn.go:39-56.
func (t *BackendTransport) ExecuteTurn(ts *TurnState) error {
	a := t.agent

	be, err := a.BackendManager.Get(ts.Ctx, ts.SessionKey)
	if err != nil {
		return err
	}
	ts.Backend = be

	_, err = be.SendTurn(ts.Ctx, ts.Prompt, &backend.EventHandler{})
	if err != nil {
		return err
	}

	// Wait for turn completion asynchronously. The watcher's persistent
	// handler calls notifyTurnComplete when it sees end_turn in the JSONL.
	// WaitForTurn blocks until that signal arrives.
	// NOTE: WaitForTurn uses a single per-backend channel — only one
	// waiter is supported at a time. Stage 5 replaces this with per-turn
	// callbacks for proper concurrency.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := be.WaitForTurn(ctx); err == nil {
			close(ts.CompletionChan)
		}
		// On timeout, CompletionChan stays open; runPostTurn's own 10min
		// timeout will log the warning.
	}()

	return nil
}

// --- Phase 4: Post-turn ---

// SaveSession is a no-op — CC owns its session file.
func (t *BackendTransport) SaveSession(ts *TurnState) error { return nil }

// UpdateSessionMeta is a stub — Stage 5 adds TurnResult.Usage to the
// watcher so we can update cost/token tracking from the JSONL data.
func (t *BackendTransport) UpdateSessionMeta(ts *TurnState) {
	// TODO(stage5): read ts.FinalUsage from watcher's extracted usage
	// and update ts.SessionMeta.prevCost, prevInput, etc.
}

// RunCompaction is a stub — will send /compact command to CC when
// context window usage exceeds threshold. Needs watcher usage data (Stage 5).
func (t *BackendTransport) RunCompaction(ts *TurnState) {
	// TODO(stage5): check usage against threshold, send be.SendCommand("/compact")
}
