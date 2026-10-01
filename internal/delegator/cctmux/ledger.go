// ledger.go — the tmux Claude Code backend's cost path (#2111 P2): every
// main-thread call its session watcher reads is booked once its usage is final
// (the line carrying its stop_reason), keyed by the message id, on the foci
// turn open when it completed.
//
// This backend reads only the main transcript, so a subagent's calls (in their
// own transcripts) and CC's utility calls are not booked; the stream-json
// backend (ccstream, ccbook.go) books both. A call interrupted before its
// stop_reason is not booked either.

package cctmux

import (
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/delegator/ccstream"
)

// ledgerID is what this backend's calls are booked under, set at Start.
type ledgerID struct{ session, agentID string }

// LedgerBackend implements delegator.LedgerBooker: this backend books its own
// calls, so the agent layer records only its turns.
func (b *Backend) LedgerBackend() string { return accounting.BackendCCStream }

// onAssistantLine books one main-transcript assistant line if it completes a
// call. A copy of a call already booked (CC re-appends history after a
// compaction) is a no-op in the ledger, whose key is the message id.
func (b *Backend) onAssistantLine(raw []byte, path string) {
	c, ok := ccstream.ParseTranscriptCall(raw)
	if !ok || !c.Complete || c.ID == "" {
		return
	}
	turn := b.ledgerTurn(c)
	_, err := accounting.Live().RecordCall(turn, accounting.Call{
		Key: c.ID, Backend: accounting.BackendCCStream, Provider: "anthropic", Model: c.Model,
		Session: turn.Session, AgentID: turn.AgentID, TurnID: turn.TurnID,
		Kind: accounting.KindCall, Finality: accounting.FinalityCompleted, ClassMethod: c.ClassMethod,
		BilledAt: turn.StartedAt, Tokens: c.Tokens, StopReason: c.StopReason, SessionFile: path,
	}, nil)
	if err != nil {
		b.logger().Errorf("ledger: book %s: %v", c.ID, err)
	}
}

// ledgerTurn is the turn a call completing now is booked on: the foci turn
// open on this backend, whose facts the agent layer recorded when it began, or
// with none open (a run CC started itself) a turn of the call's own. Its
// StartedAt is the call's billing time.
func (b *Backend) ledgerTurn(c ccstream.TranscriptCall) accounting.Turn {
	// Not under b.mu: the watcher calls in holding its own lock, and Close
	// takes b.mu before stopping the watcher.
	var session, agentID string
	if id := b.ledgerID.Load(); id != nil {
		session, agentID = id.session, id.agentID
	}
	if session == "" {
		session = accounting.NoSession
	}
	billed := c.At
	if billed.IsZero() {
		billed = time.Now().UTC()
	}
	b.turnMu.Lock()
	var turnID string
	if b.turnEvents != nil {
		turnID = b.turnEvents.TurnID
	}
	b.turnMu.Unlock()
	t := accounting.Turn{TurnID: turnID, Session: session, AgentID: agentID,
		Backend: accounting.BackendCCStream, Source: accounting.SourceUser, StartedAt: billed}
	if turnID == "" {
		t.TurnID = accounting.MintTurnID(session, "run", billed)
		t.Source = accounting.SourceAutonomous
		t.EndedAt, t.ActivityClosedAt = billed, billed
	}
	return t
}
