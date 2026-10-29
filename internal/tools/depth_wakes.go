package tools

// Wake ownership across a depth tree (#2647, Dick 2026-10-29: "If a leaf sets
// a wake then closes, the wake should bubble up to parents/root, like spawns
// do"). A wake (foci_remind wake=true) keeps the session that set it; when it
// fires, its owner is resolved by the same walk as a spawn's results
// (SpawnRoute): the leaf while it is open, else the node it handed over to,
// up to the root chat. While the route holds (the leaf has reported but its
// parent has not taken the report through its fan-in) the wake waits for the
// next handover point. The pending-wake indicator lists the wake under the
// same owner (WakeOwner).

import (
	"context"
	"fmt"
)

// AwaitWakeOwner blocks until a wake set from sessionKey may be delivered and
// returns where it goes: Owner is the session to fire into, Via names the leaf
// that set it when Owner is another session. ok is false when ctx ends first
// (the wake was cancelled while held).
func (m *DepthManager) AwaitWakeOwner(ctx context.Context, sessionKey string) (r SpawnRoute, ok bool) {
	for {
		sig := m.HandoverSignal()
		r = m.SpawnRoute(sessionKey)
		if r.Owner == "" {
			r.Owner = sessionKey
		}
		if !r.Hold {
			return r, true
		}
		select {
		case <-sig:
		case <-ctx.Done():
			return r, false
		}
	}
}

// AwaitWakeOwner is DepthManager.AwaitWakeOwner on sessionKey's agent; a
// session of an agent without depth mode owns its own wakes.
func AwaitWakeOwner(ctx context.Context, sessionKey string) (SpawnRoute, bool) {
	if m := DepthManagerFor(sessionKey); m != nil {
		return m.AwaitWakeOwner(ctx, sessionKey)
	}
	return SpawnRoute{Owner: sessionKey}, true
}

// WakeOwner returns the session that owns a wake set from sessionKey now: the
// one the wake would fire into, or, while the route holds, the leaf that set
// it. Used for the pending-wake indicator (#2600), so it moves with the wake.
func (m *DepthManager) WakeOwner(sessionKey string) string {
	if r := m.SpawnRoute(sessionKey); r.Owner != "" {
		return r.Owner
	}
	return sessionKey
}

// WakeOwner is DepthManager.WakeOwner on sessionKey's agent; a session of an
// agent without depth mode owns its own wakes.
func WakeOwner(sessionKey string) string {
	if m := DepthManagerFor(sessionKey); m != nil && sessionKey != "" {
		return m.WakeOwner(sessionKey)
	}
	return sessionKey
}

// ReparentedWakeNote heads a wake delivered to a session that did not set it;
// via names the depth leaf that did (SpawnRoute.Via).
func ReparentedWakeNote(via string) string {
	return fmt.Sprintf("[REPARENTED WAKE] This wake was set by %s, which has since closed; it now comes to you.", via)
}
