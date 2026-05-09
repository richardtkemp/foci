package agent

import (
	"fmt"
	"time"
)

// sessionMetaLastActivity is the session_metadata key holding the unix
// timestamp of the most recent turn executed under this session base,
// regardless of trigger (user, cron, CLI, webhook, agent-to-agent,
// system-injected). Written from OrchestrateFullTurn so every turn-init
// path participates uniformly. Used by --if-active / --if-inactive gates
// to answer "has this session been doing anything recently?" (TODO #753).
//
// Stored against SessionKeyBase(sessionKey) — the {agentID}/{type}{id}
// prefix that is stable across compaction (version rotation) and branching.
// CLI sends target the agent's *main* session, so the gate consults that
// session's row; activity in branches or sub-agents does not falsely keep
// the main session "warm" for keepalive purposes.
//
// Distinct from "last_user_activity" (still in agent_metadata, written by
// internal/telegram/bot_receive.go and internal/discord/receive.go) which
// tracks primary-bot user messages only — that key is deliberately untouched
// to preserve the user-attention gate's "cron cannot defeat itself" property.
const sessionMetaLastActivity = "last_activity"

// IsTurnInFlight returns true if any turn is currently executing under
// OrchestrateFullTurn for the given session base — covers both API and
// delegated transports. Distinct from IsProcessing, which only reflects the
// API path's internal counter and is per-agent rather than per-session.
//
// `base` is SessionKeyBase(ts.SessionKey) — the stable {agentID}/{type}{id}
// prefix. Branches of a session share the parent's base, so a turn running
// in a branch correctly registers as in-flight under the main session.
//
// This is the runtime signal used by the activity gate to short-circuit
// keepalive sends while a turn is mid-flight (e.g. blocked waiting for a
// permission decision in the delegated path).
func (a *Agent) IsTurnInFlight(base string) bool {
	a.inFlightMu.Lock()
	defer a.inFlightMu.Unlock()
	return a.inFlight[base] > 0
}

// markInFlight increments the in-flight counter for the given session base
// and returns a one-shot decrement closure. The closure is safe to call
// exactly once; subsequent calls are no-ops via the local guard.
//
// Usage at call sites:
//
//	base := session.SessionKeyBase(ts.SessionKey)
//	done := a.markInFlight(base)
//	defer done()
//
// The orchestrator pairs this with touchLastActivity at turn entry so that
// both signals — runtime "doing something now" and persistent "did something
// at time T" — track every turn-init path through the single chokepoint.
func (a *Agent) markInFlight(base string) func() {
	a.inFlightMu.Lock()
	if a.inFlight == nil {
		a.inFlight = make(map[string]int32)
	}
	a.inFlight[base]++
	a.inFlightMu.Unlock()

	var done bool
	return func() {
		if done {
			return
		}
		done = true
		a.inFlightMu.Lock()
		defer a.inFlightMu.Unlock()
		if a.inFlight == nil {
			return
		}
		a.inFlight[base]--
		if a.inFlight[base] <= 0 {
			delete(a.inFlight, base)
		}
	}
}

// touchLastActivity writes the current unix timestamp to the session base's
// last_activity metadata key, recording that this session is currently
// executing a turn. No-op if SessionIndex is nil (test agents) or base is
// empty.
//
// Errors are swallowed (logged at debug, not warn) — a transient DB write
// failure should not abort the turn. The next turn refreshes the timestamp.
func (a *Agent) touchLastActivity(base string) {
	if a.SessionIndex == nil || base == "" {
		return
	}
	val := fmt.Sprintf("%d", time.Now().Unix())
	if err := a.SessionIndex.SetSessionMetadata(base, sessionMetaLastActivity, val); err != nil {
		a.logger().Debugf("touchLastActivity: SetSessionMetadata(%s, %s, %s): %v",
			base, sessionMetaLastActivity, val, err)
	}
}
