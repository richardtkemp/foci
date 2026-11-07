package main

import (
	"strings"
	"sync"
	"testing"
	"time"

	"foci/internal/delegator/accounting"
	"foci/internal/platform"
)

// alertHub is an app-shaped connection manager for the #2933 tests: every
// session resolves to a connection bound to it (as the real hub's BotForSession
// does), but a notification shows only in a session with a conversation; the
// others return no message id, exactly as appConn.notify does.
type alertHub struct {
	stubConnMgr
	mu      sync.Mutex
	bound   map[string]bool
	shownIn []string
	texts   []string
}

func (h *alertHub) ForSessionOrPrimary(sessionKey, _ string) platform.Connection {
	return &alertConn{stubConn: stubConn{sessionKey: sessionKey}, hub: h}
}

type alertConn struct {
	stubConn
	hub *alertHub
}

func (c *alertConn) PlatformName() string { return "app" }

func (c *alertConn) SendNotificationDirect(text string) string {
	c.hub.mu.Lock()
	defer c.hub.mu.Unlock()
	if !c.hub.bound[c.sessionKey] {
		return ""
	}
	c.hub.shownIn = append(c.hub.shownIn, c.sessionKey)
	c.hub.texts = append(c.hub.texts, text)
	return "n-1"
}

func alertDeps(hub *alertHub, defaultKey string) backendPromptDeps {
	return backendPromptDeps{connMgr: hub, agentID: "main", defaultSession: func() string { return defaultKey }}
}

func bustReport(sess string) accounting.CacheReport {
	return accounting.CacheReport{
		Session: sess, AgentID: "main", Model: "claude-opus-4-5", PrevModel: "claude-opus-4-5",
		Class: accounting.CacheClassBust, Cause: accounting.CacheCausePrefixChanged,
		Expected: 98_765, Read: 12_345, Lost: 86_420, Write: 86_500,
		RewriteUSD: 0.54, ReadUSD: 0.04, Priced: true,
	}
}

// The session's own chat shows the alert when it can.
func TestCacheAlert_OwnChat(t *testing.T) {
	t.Parallel()
	hub := &alertHub{bound: map[string]bool{"main/c1": true, "main/c9": true}}
	newCacheAlertSink(alertDeps(hub, "main/c9"), nil)(bustReport("main/c1"))
	if len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c1" {
		t.Fatalf("alert shown in %v, want [main/c1]", hub.shownIn)
	}
	if strings.HasPrefix(hub.texts[0], "↪️") {
		t.Errorf("own-chat alert carries a redirect note: %q", hub.texts[0])
	}
	for _, want := range []string{"Cache bust", "12.3k of 98.8k", "86.4k rewritten", "$0.54", "main/c1"} {
		if !strings.Contains(hub.texts[0], want) {
			t.Errorf("alert %q lacks %q", hub.texts[0], want)
		}
	}
}

// A fork with no conversation of its own walks up to its root's chat, and the
// alert names the session it is about.
func TestCacheAlert_BranchWalksUp(t *testing.T) {
	t.Parallel()
	hub := &alertHub{bound: map[string]bool{"main/c1": true, "main/c9": true}}
	newCacheAlertSink(alertDeps(hub, "main/c9"), nil)(bustReport("main/c1/b100"))
	if len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c1" {
		t.Fatalf("alert shown in %v, want [main/c1] (the root, before the default)", hub.shownIn)
	}
	if !strings.HasPrefix(hub.texts[0], "↪️ **From session main/c1/b100**") {
		t.Errorf("redirected alert = %q, want it to name the session first", hub.texts[0])
	}
}

// No chat on the tree shows it: the agent's default chat does.
func TestCacheAlert_DefaultChat(t *testing.T) {
	t.Parallel()
	hub := &alertHub{bound: map[string]bool{"main/c9": true}}
	newCacheAlertSink(alertDeps(hub, "main/c9"), nil)(bustReport("main/c1/b100"))
	if len(hub.shownIn) != 1 || hub.shownIn[0] != "main/c9" {
		t.Fatalf("alert shown in %v, want [main/c9]", hub.shownIn)
	}
}

// cache_bust_notify off: nothing is sent; the setting is read at each bust.
func TestCacheAlert_Disabled(t *testing.T) {
	t.Parallel()
	hub := &alertHub{bound: map[string]bool{"main/c1": true}}
	on := false
	sink := newCacheAlertSink(alertDeps(hub, ""), func() bool { return on })
	sink(bustReport("main/c1"))
	if len(hub.shownIn) != 0 {
		t.Fatalf("alert sent with cache_bust_notify off: %v", hub.shownIn)
	}
	on = true
	sink(bustReport("main/c1"))
	if len(hub.shownIn) != 1 {
		t.Fatalf("alert not sent after cache_bust_notify turned on: %v", hub.shownIn)
	}
}

// Only a bust reaches a sink, and only its own agent's; an expiry does not.
func TestCacheAlert_DispatchFilters(t *testing.T) {
	got := make(chan accounting.CacheReport, 4)
	registerCacheAlert("alertagent", func(r accounting.CacheReport) { got <- r })
	t.Cleanup(func() { registerCacheAlert("alertagent", nil) })

	bust := bustReport("alertagent/c1")
	bust.AgentID = ""
	expiry := bust
	expiry.Class, expiry.Cause = accounting.CacheClassExpiry, accounting.CacheCauseExpiry
	other := bust
	other.Session = "nobody/c1"

	dispatchCacheAlert(expiry)
	dispatchCacheAlert(other)
	dispatchCacheAlert(bust)
	select {
	case r := <-got:
		if r.Class != accounting.CacheClassBust || r.Session != "alertagent/c1" {
			t.Fatalf("sink got %+v, want the bust", r)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("bust never reached its agent's sink")
	}
	select {
	case r := <-got:
		t.Fatalf("sink got a second report %+v; an expiry or another agent's bust must not reach it", r)
	default:
	}
}

func TestCacheBustCause(t *testing.T) {
	t.Parallel()
	r := bustReport("main/c1")
	r.Cause, r.PrevModel, r.Model = accounting.CacheCauseModelSwitch, "a", "b"
	if got := cacheBustCause(r); got != "model switch (a → b)" {
		t.Errorf("model switch cause = %q", got)
	}
	r.Cause, r.Source = accounting.CacheCauseForkMiss, "main/c1"
	if got := cacheBustCause(r); !strings.Contains(got, "fork of main/c1") {
		t.Errorf("fork miss cause = %q", got)
	}
}
