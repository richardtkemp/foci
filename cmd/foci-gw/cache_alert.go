package main

// Cache-bust alerts in chat (#2933). Dick, 2026-11-07: "I want cache bust
// alerts in chat. Send to the session's chat if user-visible, else walk up the
// tree to a visible session, else default chat (we have code for this ladder,
// reuse it."
//
// The cache watch (internal/delegator/accounting/cachewatch.go, #2930) reports
// every booked call whose cache read fell short of what its request should
// have read. logging_init.go logs each report and hands it to
// dispatchCacheAlert. A bust is posted to a chat; a TTL expiry is not (it is
// the cache working as designed). Each agent registers its own sink in
// wireAgentPlatformCallbacks, gated live by [notify] cache_bust_notify
// (default on, global or per agent).
//
// The chat is chosen by the family-tree walk every backend prompt and app media
// send already uses (backendPromptTargets): the session's own chat, a spawn's
// owner chain, each live ancestor, the root, then the agent's default session.
// The first chat that shows the alert takes it.

import (
	"fmt"
	"strings"
	"sync"

	"foci/internal/delegator"
	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/platform"
	"foci/internal/session"
	"foci/internal/tools"
)

// cacheAlerts maps an agent id to its cache-bust alert sink. The cache watch
// is process-wide (one hook); the sinks are per agent.
var cacheAlerts sync.Map // agentID -> func(accounting.CacheReport)

// registerCacheAlert installs (or, with a nil sink, removes) agentID's sink.
func registerCacheAlert(agentID string, sink func(accounting.CacheReport)) {
	if agentID == "" {
		return
	}
	if sink == nil {
		cacheAlerts.Delete(agentID)
		return
	}
	cacheAlerts.Store(agentID, sink)
}

// dispatchCacheAlert hands a bust to its agent's sink. The cache report hook
// runs synchronously after a booking commits, so the delivery runs on its own
// goroutine. An expiry, or a report of an agent with no sink, is dropped.
func dispatchCacheAlert(r accounting.CacheReport) {
	sink, ok := cacheAlertSinkFor(r)
	if !ok {
		return
	}
	go sink(r)
}

// cacheAlertSinkFor is the sink a report goes to: a bust's agent's, if it has
// one.
func cacheAlertSinkFor(r accounting.CacheReport) (func(accounting.CacheReport), bool) {
	if r.Class != accounting.CacheClassBust {
		return nil, false
	}
	agentID := r.AgentID
	if agentID == "" {
		agentID = session.AgentIDFromKey(r.Session)
	}
	v, ok := cacheAlerts.Load(agentID)
	if !ok {
		return nil, false
	}
	return v.(func(accounting.CacheReport)), true
}

// newCacheAlertSink is one agent's sink: when enabled() (read at each bust, so
// the setting is live) it posts the alert up the family tree from the report's
// session, and logs where it landed.
func newCacheAlertSink(d backendPromptDeps, enabled func() bool) func(accounting.CacheReport) {
	logger := log.NewComponentLogger("agent:" + d.agentID)
	return func(r accounting.CacheReport) {
		if enabled != nil && !enabled() {
			return
		}
		target, ok := notifyUpTree(d, r.Session, cacheBustAlertText(r), "alert")
		switch {
		case !ok:
			logger.Warnf("cache bust alert for session %s not delivered: no chat on the walk could show it", r.Session)
		case target != r.Session:
			logger.Infof("cache bust alert for session %s shown in %s (its own chat could not show it)", r.Session, target)
		}
	}
}

// notifyUpTree posts a notification to the first chat on the family-tree walk
// from sessionKey (backendPromptTargets) that shows it, and returns that
// chat's session. A chat shows it when its send returns a message id: an app
// session with no conversation of its own (a fork, a spawn) returns none, so
// the walk moves on. A session whose platform confines replies to its own
// conversation (Slack) is offered only its own chat: one user's notice is
// never copied to another audience. what names the thing in the origin note a
// redirected notification carries ("alert").
func notifyUpTree(d backendPromptDeps, sessionKey, text, what string) (string, bool) {
	if d.connMgr == nil || sessionKey == "" {
		return "", false
	}
	targets, spawned := backendPromptTargets(d, sessionKey)
	if platform.SessionConfinesReplies(d.connMgr, sessionKey) {
		targets, spawned = []string{sessionKey}, false
	}
	for _, target := range targets {
		conn := d.connMgr.ForSessionOrPrimary(target, chatAgentID(target, d.agentID))
		if conn == nil {
			continue
		}
		body := delegator.PrependOrigin(originNote(tools.Spawns, sessionKey, target, spawned, what), text)
		var id string
		if sn, ok := conn.(platform.SessionNotifier); ok {
			id = sn.SendNotificationToSession(target, body)
		} else {
			id = conn.SendNotificationDirect(body)
		}
		if id != "" {
			return target, true
		}
	}
	return "", false
}

// cacheBustAlertText is the chat text of one bust: what was lost, what it
// cost, and the likely cause.
func cacheBustAlertText(r accounting.CacheReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Cache bust: read %s of %s cached tokens, %s rewritten",
		kTokens(r.Read), kTokens(r.Expected), kTokens(r.Lost))
	if r.Priced {
		fmt.Fprintf(&b, " ($%.2f; a hit would have cost $%.2f)", r.RewriteUSD, r.ReadUSD)
	}
	b.WriteString(". Cause: ")
	b.WriteString(cacheBustCause(r))
	b.WriteString(".")
	if r.Actor != "" {
		fmt.Fprintf(&b, " Subagent %s.", r.Actor)
	}
	fmt.Fprintf(&b, " Session %s, model %s.", r.Session, r.Model)
	return b.String()
}

// cacheBustCause words a report's cause for a person.
func cacheBustCause(r accounting.CacheReport) string {
	switch r.Cause {
	case accounting.CacheCauseModelSwitch:
		return fmt.Sprintf("model switch (%s → %s)", r.PrevModel, r.Model)
	case accounting.CacheCausePromptChanged:
		return "the system prompt changed"
	case accounting.CacheCauseForkMiss:
		return fmt.Sprintf("a fork of %s missed its cache", r.Source)
	case accounting.CacheCausePrefixChanged:
		return fmt.Sprintf("the request changed after the first %s tokens", kTokens(r.Read))
	}
	return r.Cause
}

// kTokens renders a token count compactly: 950, 12.3k, 1.2M.
func kTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	}
	return fmt.Sprintf("%d", n)
}
