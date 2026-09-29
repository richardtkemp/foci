// ledger.go — the opencode adapter of the cost ledger (#2111 P2).
//
// Every opencode assistant message is one API call. It is booked once its
// usage is final (time.completed set), keyed by the message id, on the foci
// turn open when it completed, with opencode's own per-message cost stored
// beside it as a per-call backend report. A subagent's messages (a child
// session's, rerouted by the subscriber) are booked the same way with the
// task tool's callID as the actor.

package opencode

import (
	"fmt"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/modelinfo"
)

// LedgerBackend implements delegator.LedgerBooker: this backend books its own
// calls, so the agent layer books no turn-level row for it.
func (b *Backend) LedgerBackend() string { return accounting.BackendOpencode }

// messageTokens normalises opencode's per-message counts into ledger classes.
// They are already disjoint: tokens.total is input + output + reasoning +
// cache read + cache write, so reasoning is billed ON TOP of output, at the
// output rate (#2112 P0-d). Cache writes carry no TTL.
func messageTokens(t *MessageTokens) modelinfo.Tokens {
	return modelinfo.Tokens{
		modelinfo.ClassInput:      t.Input,
		modelinfo.ClassOutput:     t.Output,
		modelinfo.ClassReasoning:  t.Reasoning,
		modelinfo.ClassCacheRead:  t.Cache.Read,
		modelinfo.ClassCacheWrite: t.Cache.Write,
	}
}

// bookMessage books one assistant message as a call once it has completed;
// an earlier, streaming update of it is skipped. actor is "" for the
// session's own messages, else the subagent (the task tool's callID). A
// replay of a booked message is a no-op in the ledger (its id is the key),
// and a replay with different counts alarms there.
func (b *Backend) bookMessage(msg Message, actor string) {
	if msg.Tokens == nil || msg.Time.Completed == 0 || msg.ID == "" {
		return
	}
	billed := time.UnixMilli(msg.Time.Completed).UTC()
	tokens := messageTokens(msg.Tokens)
	kind := accounting.KindCall
	if msg.Summary || msg.Mode == "compaction" {
		kind = accounting.KindCompaction
	}
	created := billed
	if msg.Time.Created > 0 {
		created = time.UnixMilli(msg.Time.Created).UTC()
	}
	turn := b.ledgerTurn(kind, created, billed)
	detail := map[string]any{"opencode_session": msg.SessionID}
	if msg.Time.Created > 0 {
		detail["duration_ms"] = msg.Time.Completed - msg.Time.Created
	}
	cost := msg.Cost
	booked, err := accounting.Live().RecordCall(turn, accounting.Call{
		Key: msg.ID, Backend: accounting.BackendOpencode, Provider: msg.ProviderID, Model: msg.ModelID,
		Session: turn.Session, AgentID: b.agentID, TurnID: turn.TurnID, Actor: actor,
		Kind: kind, Finality: accounting.FinalityCompleted, ClassMethod: accounting.ClassMethodUnknown,
		BilledAt: billed, Tokens: tokens, StopReason: msg.Finish, Detail: detail,
	}, &accounting.Report{
		Backend: accounting.BackendOpencode, Session: turn.Session, ScopeKey: msg.ID,
		Model: msg.ModelID, Grain: accounting.GrainPerCall, At: billed, CostUSD: &cost, Tokens: tokens,
	})
	if err != nil {
		log.NewComponentLogger(b.logComponent()).Errorf("book message %s: %v", msg.ID, err)
		return
	}
	if booked.Duplicate {
		return
	}
	// opencode's own per-message figure checks our pricing table: the same
	// shared rule as every backend (#1674), fed the price the ledger's views
	// give this call.
	calculated, _ := modelinfo.CostAsOf(msg.ModelID, billed, tokens)
	b.costCheck.Check(msg.ModelID, calculated, cost,
		func() string {
			t := msg.Tokens
			return fmt.Sprintf("in=%d out=%d reasoning=%d cache_read=%d cache_write=%d",
				t.Input, t.Output, t.Reasoning, t.Cache.Read, t.Cache.Write)
		},
		log.NewComponentLogger(b.logComponent()).Warnf)
}

// ledgerTurn is the turn a message completing now is booked on: the foci turn
// open on this backend — which a subagent's messages and an in-turn automatic
// compaction belong to — or, with none open (an operator's /compact, which
// runs between turns), a turn of the message's own. The agent layer recorded
// the open turn's facts when it began (buildTurnEvents); naming it here with
// only its id never overwrites them.
func (b *Backend) ledgerTurn(kind string, created, billed time.Time) accounting.Turn {
	b.turnMu.Lock()
	var turnID string
	if b.turnActive && b.turnEvents != nil {
		turnID = b.turnEvents.TurnID
	}
	b.turnMu.Unlock()
	session := b.startOpts.SessionKey
	if session == "" {
		session = accounting.NoSession
	}
	t := accounting.Turn{
		TurnID: turnID, Session: session, AgentID: b.agentID, Backend: accounting.BackendOpencode,
		Source: accounting.SourceAutonomous, StartedAt: created,
	}
	if turnID == "" {
		// Its spend is this one message, final now.
		t.TurnID = accounting.MintTurnID(session, kind, created)
		t.EndedAt, t.ActivityClosedAt = billed, billed
		t.Source = accounting.SourceSystem
		if kind == accounting.KindCompaction {
			t.Source = accounting.SourceCompaction
		}
	}
	return t
}
