package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"foci/internal/delegator"
)

// AttachSessionEvents installs the session-scoped delivery callbacks.
func (b *Backend) AttachSessionEvents(events *delegator.SessionEvents) {
	b.sessionEvents.Store(events)
}

// ImmediateInject delivers a user-role event by calling turn/start on the
// app-server. Mid-turn folding (turn/steer) is supported for SourceSteer
// and SourceUser; SourceSystem at idle begins a new turn; in-flight
// SourceSystem returns ErrTurnInFlight.
func (b *Backend) ImmediateInject(ctx context.Context, inj delegator.Inject) error {
	switch inj.Source {
	case delegator.SourcePass:
		b.lg.Debugf("ignoring %s inject", inj.Source)
		return nil
	case delegator.SourceCompact:
		return b.triggerCompaction()
	}

	if b.IsTurnInFlight() {
		if inj.Source == delegator.SourceSteer || inj.Source == delegator.SourceUser {
			return b.steerTurn(inj.Text)
		}
		return delegator.ErrTurnInFlight
	}

	return b.beginTurn(inj.Text, inj.Turn)
}

// beginTurn starts a new turn via turn/start.
func (b *Backend) beginTurn(text string, turn *delegator.TurnEvents) error {
	b.turnMu.Lock()
	if b.turnActive {
		b.turnMu.Unlock()
		return delegator.ErrTurnInFlight
	}
	b.turnActive = true
	b.turnEvents = turn
	b.turnResultCh = make(chan *delegator.TurnResult, 1)
	b.turnText.Reset()
	b.turnTools = 0
	b.stashedUsage = nil
	b.turnMu.Unlock()

	threadID := b.SessionID()
	if threadID == "" {
		b.completeTurn(&delegator.TurnResult{Text: "codex: no active thread"})
		return fmt.Errorf("codex: no active thread")
	}

	params := turnStartParams{
		ThreadID: threadID,
		Input:    []turnInput{{Type: "text", Text: text}},
		Cwd:      b.workDir,
	}
	b.applyPendingControls(&params)

	result, err := b.sendAndWait("turn/start", params)
	if err != nil {
		b.completeTurn(&delegator.TurnResult{
			Text: fmt.Sprintf("codex turn/start failed: %v", err),
		})
		return err
	}
	if result == nil {
		b.completeTurn(&delegator.TurnResult{
			Text: "codex turn/start: no response (process exited)",
		})
		return fmt.Errorf("codex: turn/start: process exited")
	}
	if params.Model != "" {
		b.mu.Lock()
		b.model = params.Model
		b.mu.Unlock()
	}

	var tr turnStartedParams
	if err := json.Unmarshal(result, &tr); err == nil && tr.Turn.Status == "failed" {
		b.completeTurn(&delegator.TurnResult{Text: "codex turn/start: turn failed"})
		return fmt.Errorf("codex: turn/start returned failed status")
	}

	return nil
}

// steerTurn appends input to the in-flight turn via turn/steer. The docs
// say turn/steer returns the accepted turnId, so it's a request.
func (b *Backend) steerTurn(text string) error {
	threadID := b.SessionID()
	go func() {
		_, err := b.sendAndWait("turn/steer", struct {
			ThreadID string      `json:"threadId"`
			Input    []turnInput `json:"input"`
		}{
			ThreadID: threadID,
			Input:    []turnInput{{Type: "text", Text: text}},
		})
		if err != nil {
			b.lg.Warnf("turn/steer failed: %v", err)
		}
	}()
	return nil
}

// environ returns the current process environment.
func environ() []string {
	return os.Environ()
}
