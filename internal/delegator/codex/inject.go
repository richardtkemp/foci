package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
	if err := json.Unmarshal(result, &tr); err == nil {
		if tr.Turn.Status == "failed" {
			b.completeTurn(&delegator.TurnResult{Text: "codex turn/start: turn failed"})
			return fmt.Errorf("codex: turn/start returned failed status")
		}
		if tr.Turn.ID != "" {
			b.turnMu.Lock()
			b.turnID = tr.Turn.ID
			b.turnMu.Unlock()
		}
	}

	return nil
}

// steerTurn folds input into the in-flight turn via turn/steer, waiting
// synchronously for codex's accept/reject (fast — an RPC ack, not the model
// turn itself; verified live it returns before any steered content streams).
//
// Previously this ran fire-and-forget in a goroutine and ImmediateInject
// returned nil immediately, so the caller believed the message was
// delivered — but turn/steer requires expectedTurnId (a live-verified
// precondition; see turnSteerParams) and codex rejects the call with "no
// active turn to steer" once the turn completes before the steer lands.
// That rejection was only Warnf'd: the user's mid-turn message vanished
// with no re-injection and no caller-visible error.
//
// Now: a turn-already-ended rejection returns delegator.ErrTurnNotInFlight,
// the same sentinel ccstream's Inject(SourceSteer) uses for the analogous
// race — the caller (inbox.go) already re-routes that error to the normal
// idle path, which starts a properly-tracked fresh turn with the same text
// instead of losing it. Any other error (dead stdin, protocol error) is
// returned as-is; inbox.go's default case queues the text for a fresh turn
// on any non-nil error too, so no message is dropped on the floor.
func (b *Backend) steerTurn(text string) error {
	threadID := b.SessionID()
	b.turnMu.Lock()
	turnID := b.turnID
	b.turnMu.Unlock()

	_, err := b.sendAndWait("turn/steer", turnSteerParams{
		ThreadID:       threadID,
		ExpectedTurnID: turnID,
		Input:          []turnInput{{Type: "text", Text: text}},
	})
	if err != nil {
		if isNoActiveTurnError(err) {
			b.lg.Debugf("turn/steer raced turn completion (turnId=%s): %v", turnID, err)
			return delegator.ErrTurnNotInFlight
		}
		b.lg.Warnf("turn/steer failed: %v", err)
		return err
	}
	return nil
}

// isNoActiveTurnError reports whether err is codex's rejection for a
// turn/steer whose expectedTurnId no longer matches the active turn (the
// turn completed in the race window between the in-flight check and the
// steer landing). Message text verified live against codex 0.144.5.
func isNoActiveTurnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no active turn to steer")
}

// environ returns the current process environment.
func environ() []string {
	return os.Environ()
}
