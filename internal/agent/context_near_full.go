package agent

import (
	"fmt"

	"foci/internal/display"
	"foci/internal/session"
)

// contextNearFullPercent is where the near-full warning fires: a share of the
// FULL context window, not of the compaction threshold (Dick, 2026-11-15,
// #1306: "Warning at 95% of full context, in chat as a system message").
const contextNearFullPercent = 95

// warnIfContextNearFull posts a system notice in the session's chat when the
// turn that just ended left the context at or above contextNearFullPercent of
// the session's window. It fires once per crossing: the latch on the session's
// meta clears when a later turn ends below the line (after a compaction, say),
// so the next crossing warns again.
//
// It runs in the post-turn phase just before the compaction check, so it sees
// the turn's own final fill. When that compaction then runs, the notice is
// followed by the compaction's own lines; when it does not (no_compact,
// deferred behind background work, declined, failed), the notice is the only
// sign that the window is nearly full.
//
// Only a root chat session is warned: a spawn, branch or batch child, or an
// independent session, has no chat of its own, and its notice would land in
// the agent's default chat.
func (a *Agent) warnIfContextNearFull(ts *TurnState) {
	notify := a.ContextNearFullNoticeFunc
	if notify == nil || ts == nil || ts.FinalUsage == nil || ts.SessionMeta == nil {
		return
	}
	k, err := session.ParseSessionKey(ts.SessionKey)
	if err != nil || k.Type != 'c' || !k.IsRoot() {
		return
	}
	limit := a.SessionContextLimit(ts.SessionKey)
	if limit <= 0 {
		return
	}
	// Last-cycle context size (see DelegatedTransport.RunCompaction): the
	// final cycle's fill, not a sum over the turn's cycles.
	u := ts.FinalUsage
	fill := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	over := int64(fill)*100 >= int64(limit)*contextNearFullPercent

	a.metaMu.Lock()
	warned := ts.SessionMeta.nearFullWarned
	ts.SessionMeta.nearFullWarned = over
	a.metaMu.Unlock()
	if !over || warned {
		return
	}

	pct := int(int64(fill) * 100 / int64(limit))
	text := fmt.Sprintf("⚠️ Context is %d%% full: %s of %s tokens.",
		pct, display.FormatCommas(fill), display.FormatCommas(limit))
	a.logger().Infof("session=%s context near full: %d/%d tokens (%d%%), notice sent", ts.SessionKey, fill, limit, pct)
	// Its own goroutine: a slow platform must not hold the post-turn phase.
	go notify(ts.SessionKey, text)
}
