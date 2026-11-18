package main

import (
	"strings"
	"testing"
	"time"

	"foci/internal/agent"
	"foci/internal/delegator/accounting"
	"foci/internal/log"
	"foci/internal/warnings"
)

// TestCacheBust_NotifyOffKeepsWarnOutOfChat (#3161): an agent whose [notify]
// cache_bust_notify is off must get no chat message for its cache busts by any
// path. The #2933 alert sink honoured the key, but the bust's WARN log line
// still went through the log warning hook into every agent's chat warning
// queue and reached the app as "[system diagnostics]". The line now stays out
// of every chat queue while the key is off (read live), still reaches the
// agent queues, and another agent's bust and other WARNs reach chat as before.
func TestCacheBust_NotifyOffKeepsWarnOutOfChat(t *testing.T) {
	// The chat-warning receiver: a different agent (Cam showed codex's busts).
	recv := &agent.Agent{
		WarningQueue:     warnings.NewQueue(100, time.Hour),
		ChatWarningQueue: warnings.NewQueue(100, time.Hour),
	}
	log.SetWarnHook(func(level log.Level, component, msg string) {
		pushLogWarning(recv, level, component, msg)
	})
	t.Cleanup(func() { log.SetWarnHook(nil) })

	on := false
	registerCacheAlert("t3161muted", func(accounting.CacheReport) {}, func() bool { return on })
	t.Cleanup(func() { registerCacheAlert("t3161muted", nil, nil) })
	registerCacheAlert("t3161loud", func(accounting.CacheReport) {}, func() bool { return true })
	t.Cleanup(func() { registerCacheAlert("t3161loud", nil, nil) })

	muted := bustReport("t3161muted/ixspawn/s1")
	muted.AgentID = "t3161muted"
	loud := bustReport("t3161loud/c1")
	loud.AgentID = "t3161loud"

	logCacheReport(muted)
	logCacheReport(loud)
	log.Warnf("t3161other", "an unrelated warning t3161")

	// Other tests in the package log WARNs into the same hook: count only
	// this test's lines.
	count := func(lines []string, needle string) int {
		n := 0
		for _, l := range lines {
			n += strings.Count(l, needle)
		}
		return n
	}
	chat := recv.ChatWarnings().Drain()
	agentQ := recv.Warnings().Drain()
	if n := count(chat, "t3161muted/ixspawn/s1"); n != 0 {
		t.Errorf("muted agent's bust reached the chat warning queue %d time(s): %q", n, chat)
	}
	if n := count(chat, "t3161loud/c1"); n != 1 {
		t.Errorf("an agent with cache_bust_notify on: its bust reached chat %d time(s), want 1: %q", n, chat)
	}
	if n := count(chat, "an unrelated warning t3161"); n != 1 {
		t.Errorf("an unrelated WARN reached chat %d time(s), want 1: %q", n, chat)
	}
	if n := count(agentQ, "t3161muted/ixspawn/s1"); n != 1 {
		t.Errorf("muted agent's bust reached the agent warning queue %d time(s), want 1 (unchanged): %q", n, agentQ)
	}

	// The key is live: turned on, the next bust reaches chat again.
	on = true
	logCacheReport(muted)
	chat = recv.ChatWarnings().Drain()
	if n := count(chat, "t3161muted/ixspawn/s1"); n != 1 {
		t.Errorf("after cache_bust_notify turned on: bust reached chat %d time(s), want 1: %q", n, chat)
	}
}

// A bust logs under "ledger/<agent>" (its AgentID, else the session key's);
// one with no agent at all under plain "ledger".
func TestCacheBustLogComponent(t *testing.T) {
	t.Parallel()
	r := bustReport("codex/ixspawn/x")
	r.AgentID = ""
	if got := cacheBustLogComponent(r); got != "ledger/codex" {
		t.Errorf("component = %q, want ledger/codex", got)
	}
	r.AgentID = "cam"
	if got := cacheBustLogComponent(r); got != "ledger/cam" {
		t.Errorf("component = %q, want ledger/cam", got)
	}
	if cacheBustChatMuted("ledger") || cacheBustChatMuted("ledger/") || cacheBustChatMuted("ledgerx/a") {
		t.Error("a component with no registered agent gate must not be muted")
	}
}
