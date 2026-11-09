package agent

import (
	"time"

	"foci/internal/delegator"
)

// Message info (#2992): two facts the app's Message info shows are recorded
// here, at the moment the agent decides them, instead of being worked out later
// from timestamps.
//
//   - The live turn of each session: the ledger id (TurnState.RowID) of the
//     turn running on it, from the moment its input goes to the model until it
//     ends. A spawn started from the session records it as its parent turn
//     (LiveTurnID), and a message that steers or joins a running turn names it.
//   - How a user message reached the agent (OnInputDelivery): the outcome is
//     reported where the inbox and the turn transport choose it.

// Input delivery outcomes, reported through Agent.OnInputDelivery.
const (
	// DeliveryQueued: the message waited because a turn was running (or a
	// hold was on) when it arrived. A later DeliveryStarted for the same
	// message names the turn it then began; the waiting is still the story.
	DeliveryQueued = "queued"
	// DeliveryStarted: the message began a turn of its own.
	DeliveryStarted = "started"
	// DeliveryJoined: the message went into a turn already running as its
	// follow-up (a delegated fold) or answered the running turn's prompt.
	DeliveryJoined = "joined"
	// DeliverySteered: the message was steered into the running turn mid-way
	// (an urgent steer, or the API steer buffer drained at a tool boundary).
	DeliverySteered = "steered"
	// DeliveryPlanFeedback: the message was consumed as feedback that
	// rejected the running turn's plan.
	DeliveryPlanFeedback = "plan_feedback"
)

// setLiveTurn records turnID as the turn running on sk.
func (a *Agent) setLiveTurn(sk, turnID string) {
	if sk == "" || turnID == "" {
		return
	}
	a.liveTurns.Store(sk, turnID)
}

// clearLiveTurn forgets sk's live turn, only if it is still turnID: a turn
// that folded into another, or ended after a newer one began, leaves the
// newer one in place.
func (a *Agent) clearLiveTurn(sk, turnID string) {
	if sk == "" || turnID == "" {
		return
	}
	a.liveTurns.CompareAndDelete(sk, turnID)
}

// LiveTurnID is the ledger id of the turn running on sessionKey, or "" when
// none is (or this process did not start or adopt it).
func (a *Agent) LiveTurnID(sessionKey string) string {
	v, _ := a.liveTurns.Load(sessionKey)
	s, _ := v.(string)
	return s
}

// reportDelivery tells OnInputDelivery how refs reached the agent. No-op
// without refs (input from a surface that names no message) or a hook.
func (a *Agent) reportDelivery(sk string, refs []delegator.InputRef, outcome, turnID string) {
	if len(refs) == 0 || a.OnInputDelivery == nil {
		return
	}
	a.OnInputDelivery(sk, refs, outcome, turnID, time.Now())
}

// envRefs is env's message ref as a slice, nil when it names none.
func envRefs(env Envelope) []delegator.InputRef {
	if env.Ref.MessageID == "" {
		return nil
	}
	return []delegator.InputRef{env.Ref}
}
