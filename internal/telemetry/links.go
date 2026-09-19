package telemetry

import (
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Cross-agent linking. Langfuse has no cross-trace edge, so the two turns of
// a send_to_session exchange are joined by metadata: the CALLER's tool span
// names target_session (turn.go), and the TARGET's injected turn records
// parent_trace_id / parent_turn_id / caller_session. The target learns its
// caller through a pending-link registry keyed by target session: the tool
// stages a link when it enqueues the inject, and the next injected turn on
// that session (trigger session_notify / async_notify) consumes it. Injects
// are queued in order on the session's inbox, so the pairing holds unless
// another injection was already queued ahead — a stale link expires rather
// than mis-attaching.

// Link names the turn that caused an injected turn.
type Link struct {
	TraceID     trace.TraceID
	TurnID      string
	FromSession string
	FromAgent   string
	at          time.Time
}

const linkTTL = 10 * time.Minute

var (
	linkMu  sync.Mutex
	active  = map[string]string{} // session → in-flight turn id
	last    = map[string]string{} // session → most recently completed turn id
	pending = map[string][]Link{} // target session → staged links, FIFO
)

func setActiveTurn(session, turnID string) {
	linkMu.Lock()
	active[session] = turnID
	linkMu.Unlock()
}

func clearActiveTurn(session, turnID string) {
	linkMu.Lock()
	if active[session] == turnID {
		delete(active, session)
	}
	last[session] = turnID
	linkMu.Unlock()
}

// LinkPending stages fromSession's current turn (or, if it has none in
// flight, its last completed one) as the parent of the next injected turn on
// targetSession. No-op when tracing is off or fromSession has no known turn.
func LinkPending(targetSession, fromSession string) {
	if !Enabled() || targetSession == "" || fromSession == "" {
		return
	}
	linkMu.Lock()
	defer linkMu.Unlock()
	turnID := active[fromSession]
	if turnID == "" {
		turnID = last[fromSession]
	}
	if turnID == "" {
		return
	}
	pending[targetSession] = append(pending[targetSession], Link{
		TraceID:     TraceIDForTurn(turnID),
		TurnID:      turnID,
		FromSession: fromSession,
		FromAgent:   agentFromSession(fromSession),
		at:          time.Now(),
	})
}

// takePendingLink pops the oldest live link for session, for triggers that
// carry an injected message from another session.
func takePendingLink(session, trigger string) *Link {
	switch trigger {
	case "session_notify", "async_notify", "ask_grader":
	default:
		return nil
	}
	linkMu.Lock()
	defer linkMu.Unlock()
	q := pending[session]
	cutoff := time.Now().Add(-linkTTL)
	for len(q) > 0 {
		l := q[0]
		q = q[1:]
		if l.at.Before(cutoff) {
			continue
		}
		if len(q) == 0 {
			delete(pending, session)
		} else {
			pending[session] = q
		}
		return &l
	}
	delete(pending, session)
	return nil
}
